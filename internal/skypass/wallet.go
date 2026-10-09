package skypass

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/googlewallet"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var (
	// ErrWalletOff: SkyPass in Google Wallet is not configured
	// (SKYPASS_GOOGLE_WALLET_ENABLED is not true).
	ErrWalletOff = errors.New("skypass: google wallet is off")
	// ErrWalletUpstream: Google did not take a write core needed.
	ErrWalletUpstream = errors.New("skypass: google wallet is unavailable")
	// ErrWalletCodeUsed: a genuine Wallet code of a step a check-in
	// already took, or an older one.
	ErrWalletCodeUsed = errors.New("skypass: wallet code already used")
	// ErrWalletRateLimited: too many wrong codes for one pass.
	ErrWalletRateLimited = errors.New("skypass: too many wrong wallet codes")
)

// WalletRateLimitError is ErrWalletRateLimited with the time until the
// pass's codes are looked at again.
type WalletRateLimitError struct {
	RetryAfter time.Duration
}

func (e *WalletRateLimitError) Error() string        { return ErrWalletRateLimited.Error() }
func (e *WalletRateLimitError) Is(target error) bool { return target == ErrWalletRateLimited }

// GoogleWalletAPI is what the Wallet needs of Google (googlewallet.Client).
type GoogleWalletAPI interface {
	SaveURL(origins []string, objects []googlewallet.ObjectRef) (string, error)
	EnsureGenericClass(ctx context.Context, class googlewallet.GenericClass) error
	InsertGenericObject(ctx context.Context, object googlewallet.GenericObject) error
	UpdateGenericObject(ctx context.Context, object googlewallet.GenericObject) error
}

// WalletStatus tells an app whether to offer "Add to Google Wallet".
type WalletStatus struct {
	Google GoogleWalletStatus `json:"google"`
}

type GoogleWalletStatus struct {
	// Available is false while Google Wallet is off: hide the button.
	Available bool `json:"available"`
	// Issued is true when the person has a pass that opens the door. It
	// does not say whether a phone saved it.
	Issued bool `json:"issued"`
}

// WalletLink is the person's "Add to Google Wallet" link.
type WalletLink struct {
	SaveURL string `json:"saveUrl"`
}

// Defaults of WalletOptions.
const (
	// DefaultWalletFailureLimit wrong codes from one scanner for one pass
	// per DefaultWalletFailureWindow, then that scanner's codes for that
	// pass are refused until the window ends. A guess is one in about 33
	// million (three valid 8-digit codes), and a right one only checks the
	// pass's holder in, which door staff can do by name anyway.
	DefaultWalletFailureLimit  = 10
	DefaultWalletFailureWindow = 10 * time.Minute
	// DefaultWalletLinkTimeout bounds one link or revoke as a whole: the
	// token, the class, revoked passes, the object, each up to
	// googlewallet.DefaultTimeout.
	DefaultWalletLinkTimeout = 20 * time.Second
)

// walletRaceTimeout bounds the second look after a link's write and the
// withdrawal of a pass that ended meanwhile. It runs past the link's own
// deadline, which may be what ended the write.
const walletRaceTimeout = 15 * time.Second

// walletRaceBackoff is the wait before each further try of that
// withdrawal.
var walletRaceBackoff = []time.Duration{500 * time.Millisecond, 2 * time.Second}

type WalletOptions struct {
	Now           func() time.Time
	Logf          func(format string, args ...any)
	FailureLimit  int
	FailureWindow time.Duration
}

// Wallet is SkyPass in Google Wallet (docs/skypass-google-wallet.md): it
// issues a Member's pass, checks its rotating codes at the door, and
// withdraws it. Built without a client (Google Wallet off) it issues
// nothing and takes no code, and still withdraws: a pass left from when it
// was on stops opening the door, and erasure says what waits for Google.
type Wallet struct {
	config   GoogleWalletConfig
	api      GoogleWalletAPI
	store    WalletStore
	users    user.Store
	now      func() time.Time
	logf     func(string, ...any)
	failures *walletFailures
	metrics  *walletMetrics

	linkTimeout     time.Duration
	withdrawBackoff []time.Duration
	classReady      atomic.Bool
}

func NewWallet(config GoogleWalletConfig, api GoogleWalletAPI, store WalletStore, users user.Store, opts WalletOptions) *Wallet {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.FailureLimit <= 0 {
		opts.FailureLimit = DefaultWalletFailureLimit
	}
	if opts.FailureWindow <= 0 {
		opts.FailureWindow = DefaultWalletFailureWindow
	}
	w := &Wallet{
		config: config, api: api, store: store, users: users,
		now: opts.Now, logf: opts.Logf,
		failures:    &walletFailures{limit: opts.FailureLimit, window: opts.FailureWindow, entries: map[string]walletFailure{}},
		linkTimeout: DefaultWalletLinkTimeout, withdrawBackoff: walletRaceBackoff,
	}
	w.metrics = newWalletMetrics(w.Enabled())
	return w
}

// Enabled reports whether passes are issued and their codes taken.
func (w *Wallet) Enabled() bool {
	return w != nil && w.config.Enabled && w.api != nil && w.store != nil && len(w.config.TOTPKey) == 32
}

func (w *Wallet) status(ctx context.Context, userID uuid.UUID) (WalletStatus, error) {
	out := WalletStatus{Google: GoogleWalletStatus{Available: w.Enabled()}}
	if !w.Enabled() {
		return out, nil
	}
	_, issued, err := w.store.ActivePass(ctx, userID)
	if err != nil {
		return WalletStatus{}, err
	}
	out.Google.Issued = issued
	return out, nil
}

// googleLink writes the person's pass to Google with their current name and
// skyNumber (a new pass when they have none) and signs its save link.
func (w *Wallet) googleLink(ctx context.Context, u user.User) (WalletLink, error) {
	if !w.Enabled() {
		if w != nil {
			w.metrics.links.add("off")
		}
		return WalletLink{}, ErrWalletOff
	}
	ctx, cancel := context.WithTimeout(ctx, w.linkTimeout)
	defer cancel()
	if err := w.ensureClass(ctx); err != nil {
		w.metrics.links.add("failed")
		w.logf("skypass google wallet: class %s: %v", w.config.ClassID(), err)
		return WalletLink{}, ErrWalletUpstream
	}
	// Passes the person revoked while Google was down are withdrawn now;
	// they already open nothing, so a failure here does not stop the link.
	if passes, err := w.store.PassesOf(ctx, u.ID); err == nil {
		var revoked []WalletPass
		for _, p := range passes {
			if p.RevokedAt != nil {
				revoked = append(revoked, p)
			}
		}
		if len(revoked) > 0 {
			if _, err := w.withdrawPasses(ctx, revoked, "retry"); err != nil {
				w.logf("skypass google wallet: withdrawing %d revoked passes: %v", len(revoked), err)
			}
		}
	}
	pass, ok, err := w.store.ActivePass(ctx, u.ID)
	if err != nil {
		return WalletLink{}, err
	}
	if !ok {
		passID, err := newWalletPassID()
		if err != nil {
			return WalletLink{}, err
		}
		if pass, err = w.store.CreatePass(ctx, u.ID, passID, w.now().UTC()); err != nil {
			return WalletLink{}, err
		}
	}
	object := w.passObject(pass, u)
	writeErr := w.api.InsertGenericObject(ctx, object)
	if errors.Is(writeErr, googlewallet.ErrConflict) {
		// Saved before: the name or skyNumber may have changed since.
		writeErr = w.api.UpdateGenericObject(ctx, object)
	}
	// A revoke or an erasure may have withdrawn the pass while this write
	// was on its way, and Google may have taken this write last, even one
	// whose answer was lost: look again, and withdraw the pass once more so
	// Google keeps no name.
	after, cancelAfter := context.WithTimeout(context.WithoutCancel(ctx), walletRaceTimeout)
	defer cancelAfter()
	current, found, readErr := w.store.PassByID(after, pass.PassID)
	if readErr == nil && (!found || current.RevokedAt != nil) {
		w.withdrawRaced(after, pass)
		w.metrics.links.add("failed")
		return WalletLink{}, ErrConflict
	}
	if writeErr != nil {
		w.metrics.links.add("failed")
		w.logf("skypass google wallet: writing a pass: %v", writeErr)
		return WalletLink{}, ErrWalletUpstream
	}
	if readErr != nil {
		return WalletLink{}, readErr
	}
	link, err := w.api.SaveURL(w.config.Origins, []googlewallet.ObjectRef{{ID: object.ID, ClassID: object.ClassID}})
	if err != nil {
		w.metrics.links.add("failed")
		w.logf("skypass google wallet: signing a save link: %v", err)
		return WalletLink{}, ErrWalletUpstream
	}
	w.metrics.links.add("issued")
	return WalletLink{SaveURL: link}, nil
}

// withdrawRaced withdraws a pass a revoke or an erasure ended while its link
// was being written. Its row may be gone already (an erasure deletes it when
// Google has no object yet), and then nothing else would try again: it tries
// a few times now, and a final failure is counted
// (withdrawals_total{reason="link_race",outcome="failed"}) and logged with
// the object to withdraw by hand (docs/skypass-google-wallet.md).
func (w *Wallet) withdrawRaced(ctx context.Context, pass WalletPass) {
	var err error
	for attempt := 0; ; attempt++ {
		if _, err = w.withdrawAll(ctx, []WalletPass{pass}); err == nil {
			w.metrics.withdrawals.add("link_race,done")
			return
		}
		if attempt >= len(w.withdrawBackoff) || !sleepContext(ctx, w.withdrawBackoff[attempt]) {
			break
		}
	}
	w.metrics.withdrawals.add("link_race,failed")
	w.logf("skypass google wallet: object %s ended while its link was written and Google may still show it; withdraw it by hand: %v",
		w.objectID(pass.PassID), err)
}

// sleepContext waits d, or less if ctx ends first; it reports whether ctx is
// still live.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ensureClass writes this environment's class once per process: one user
// per pass (a forwarded link cannot put the pass on someone else's
// account), and the phone unlocked to open it. No lock is held across the
// call: two first links may both write the class, which is idempotent.
func (w *Wallet) ensureClass(ctx context.Context) error {
	if w.classReady.Load() {
		return nil
	}
	err := w.api.EnsureGenericClass(ctx, googlewallet.GenericClass{
		ID:                                     w.config.ClassID(),
		MultipleDevicesAndHoldersAllowedStatus: googlewallet.OneUserAllDevices,
		ViewUnlockRequirement:                  googlewallet.UnlockRequiredToView,
	})
	if err == nil {
		w.classReady.Store(true)
	}
	return err
}

func (w *Wallet) objectID(passID string) string {
	return w.config.IssuerID + ".sp-" + passID
}

// passObject is the pass's face: name and skyNumber, no photo (ADR-0024),
// and the rotating barcode.
func (w *Wallet) passObject(pass WalletPass, u user.User) googlewallet.GenericObject {
	name := strings.TrimSpace(strings.Join(strings.Fields(u.FirstName+" "+u.LastName), " "))
	if name == "" {
		name = "SKY LAB"
	}
	skyNumber := strings.TrimSpace(u.SkyNumber)
	caption := skyNumber
	if caption == "" {
		// Without it Google prints the barcode's value under it.
		caption = "SkyPass"
	}
	object := googlewallet.GenericObject{
		ID:          w.objectID(pass.PassID),
		ClassID:     w.config.ClassID(),
		GenericType: googlewallet.GenericTypeOther,
		State:       googlewallet.StateActive,
		CardTitle:   googlewallet.Localized("tr", "SKY LAB"),
		Subheader:   googlewallet.Localized("tr", "SkyPass"),
		Header:      googlewallet.Localized("tr", name),
		RotatingBarcode: &googlewallet.RotatingBarcode{
			Type:          googlewallet.BarcodeQRCode,
			ValuePattern:  WalletCodePrefix + pass.PassID + ":{totp_value_0}",
			AlternateText: caption,
			TotpDetails: &googlewallet.TotpDetails{
				PeriodMillis: strconv.FormatInt(WalletPeriod.Milliseconds(), 10),
				Algorithm:    googlewallet.TOTPSHA1,
				Parameters: []googlewallet.TotpParameters{{
					Key:         strings.ToUpper(hex.EncodeToString(walletSecret(w.config.TOTPKey, pass.PassID))),
					ValueLength: WalletDigits,
				}},
			},
		},
		PassConstraints: &googlewallet.PassConstraints{ScreenshotEligibility: googlewallet.ScreenshotIneligible},
	}
	if skyNumber != "" {
		object.TextModulesData = []googlewallet.TextModuleData{{ID: "sky_number", Header: "Sky numarası", Body: skyNumber}}
	}
	if w.config.LogoURL != "" {
		object.Logo = &googlewallet.Image{SourceURI: googlewallet.ImageURI{URI: w.config.LogoURL}}
	}
	return object
}

// withdrawnObject replaces a pass at Google: inactive, no name, no number,
// no barcode. The Wallet API has no delete for objects.
func (w *Wallet) withdrawnObject(passID string) googlewallet.GenericObject {
	return googlewallet.GenericObject{
		ID:          w.objectID(passID),
		ClassID:     w.config.ClassID(),
		GenericType: googlewallet.GenericTypeOther,
		State:       googlewallet.StateInactive,
		CardTitle:   googlewallet.Localized("tr", "SKY LAB"),
		Subheader:   googlewallet.Localized("tr", "Geçersiz"),
		Header:      googlewallet.Localized("tr", "SkyPass"),
	}
}

// revokeGoogle ends the person's passes (a lost phone, a reset).
func (w *Wallet) revokeGoogle(ctx context.Context, userID uuid.UUID) error {
	if !w.Enabled() {
		return ErrWalletOff
	}
	ctx, cancel := context.WithTimeout(ctx, w.linkTimeout)
	defer cancel()
	_, err := w.withdraw(ctx, userID, "member")
	return err
}

// EraseSubject withdraws every pass of a person being erased and reports
// how many it withdrew (the erase_skypass_wallet step). It fails while a
// pass is left: at Google (unreachable, or Google Wallet switched off since
// the pass was issued) or in core.
func (w *Wallet) EraseSubject(ctx context.Context, userID uuid.UUID) (int64, error) {
	if w == nil || w.store == nil {
		return 0, errors.New("skypass wallet store unavailable")
	}
	return w.withdraw(ctx, userID, "erasure")
}

func (w *Wallet) withdraw(ctx context.Context, userID uuid.UUID, reason string) (int64, error) {
	passes, err := w.store.PassesOf(ctx, userID)
	if err != nil || len(passes) == 0 {
		return 0, err
	}
	return w.withdrawPasses(ctx, passes, reason)
}

// withdrawPasses stops the passes' codes at once (revoked in core), then
// has Google forget the person on each and deletes its row. A pass Google
// does not know is done; one Google did not take stays revoked for the
// next try.
func (w *Wallet) withdrawPasses(ctx context.Context, passes []WalletPass, reason string) (int64, error) {
	done, err := w.withdrawAll(ctx, passes)
	if err != nil {
		w.metrics.withdrawals.add(reason + ",failed")
		return done, err
	}
	w.metrics.withdrawals.add(reason + ",done")
	return done, nil
}

// withdrawAll is withdrawPasses without counting.
func (w *Wallet) withdrawAll(ctx context.Context, passes []WalletPass) (int64, error) {
	now := w.now().UTC()
	for _, p := range passes {
		if p.RevokedAt == nil {
			if err := w.store.Revoke(ctx, p.PassID, now); err != nil {
				return 0, err
			}
		}
	}
	if w.api == nil || !w.config.Enabled {
		return 0, fmt.Errorf("%w: %d SkyPass wallet passes wait to be withdrawn from Google", ErrWalletOff, len(passes))
	}
	var done int64
	var first error
	for _, p := range passes {
		err := w.api.UpdateGenericObject(ctx, w.withdrawnObject(p.PassID))
		if err != nil && !errors.Is(err, googlewallet.ErrNotFound) {
			if first == nil {
				first = err
			}
			continue
		}
		if err := w.store.Delete(ctx, p.PassID); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		done++
	}
	if first != nil {
		err := fmt.Errorf("%w: %d of %d passes not withdrawn: %v", ErrWalletUpstream, int64(len(passes))-done, len(passes), first)
		if transientGoogleError(first) {
			// Google is down or busy: the erasure worker waits for it the
			// way it waits for a service, without spending its attempts.
			return done, &walletRetryLater{err: err, at: now.Add(walletRetryAfter), progressed: done > 0}
		}
		return done, err
	}
	return done, nil
}

// walletRetryAfter is when an erasure that Google kept waiting tries again.
const walletRetryAfter = 5 * time.Minute

// walletRetryLater is a withdrawal Google did not answer. RetryAt and
// Progressed are what the account erasure worker reads of a deferred
// failure.
type walletRetryLater struct {
	err        error
	at         time.Time
	progressed bool
}

func (e *walletRetryLater) Error() string      { return e.err.Error() }
func (e *walletRetryLater) Unwrap() error      { return e.err }
func (e *walletRetryLater) RetryAt() time.Time { return e.at }
func (e *walletRetryLater) Progressed() bool   { return e.progressed }

// transientGoogleError is a failure that waiting may cure: no answer, a
// timeout, 429 or 5xx. A refusal (a key Google does not take, a pass it
// finds invalid) is not: it needs a person.
func transientGoogleError(err error) bool {
	var apiErr *googlewallet.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == 0 || apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
}

// holder checks a Wallet code a scanner (scannerID) read and returns the
// active person whose pass showed it. With
// consume it also spends the code's step, atomically, so the same code (or
// an older one) checks no one in again; without it (a staff check of who
// this is) nothing is written and a spent step is still refused.
func (w *Wallet) holder(ctx context.Context, scannerID, raw string, consume bool) (user.User, error) {
	if !w.Enabled() {
		// Off, a Wallet code is just a value core does not read.
		if w != nil {
			w.metrics.codes.add("off")
		}
		return user.User{}, ErrInvalid
	}
	passID, code, ok := parseWalletCode(raw)
	if !ok {
		w.metrics.codes.add("malformed")
		return user.User{}, ErrInvalid
	}
	now := w.now()
	// Wrong codes are counted per scanner and pass: a stranger who saw the
	// pass id (every barcode shows it) cannot lock its holder out.
	budget := scannerID + "|" + passID
	if wait, blocked := w.failures.blocked(budget, now); blocked {
		w.metrics.codes.add("rate_limited")
		return user.User{}, &WalletRateLimitError{RetryAfter: wait}
	}
	pass, found, err := w.store.PassByID(ctx, passID)
	if err != nil {
		return user.User{}, err
	}
	if !found {
		w.metrics.codes.add("unknown_pass")
		return user.User{}, ErrInvalid
	}
	if pass.RevokedAt != nil {
		w.metrics.codes.add("revoked")
		return user.User{}, ErrInvalid
	}
	secret := walletSecret(w.config.TOTPKey, passID)
	step := totpCounter(now, WalletPeriod)
	matched, skew := int64(-1), 0
	for _, delta := range []int{0, -walletSkewSteps, walletSkewSteps} {
		if delta < 0 && step < uint64(-delta) {
			continue
		}
		candidate := uint64(int64(step) + int64(delta))
		if subtle.ConstantTimeCompare([]byte(hotp(secret, candidate, WalletDigits)), []byte(code)) == 1 {
			matched, skew = int64(candidate), delta
			break
		}
	}
	if matched < 0 {
		w.failures.fail(budget, now)
		w.metrics.codes.add("wrong_code")
		return user.User{}, ErrInvalid
	}
	if matched <= pass.LastCounter {
		w.metrics.codes.add("used")
		return user.User{}, ErrWalletCodeUsed
	}
	u, err := activeUser(w.users.Get(ctx, pass.UserID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			w.metrics.codes.add("inactive_account")
		}
		return user.User{}, err
	}
	if consume {
		spent, err := w.store.ConsumeCounter(ctx, passID, matched)
		if err != nil {
			return user.User{}, err
		}
		if !spent {
			// Another scan of the same code won, or the pass was revoked
			// meanwhile.
			w.metrics.codes.add("used")
			return user.User{}, ErrWalletCodeUsed
		}
	}
	w.metrics.codes.add("accepted")
	w.metrics.skew.add(strconv.Itoa(skew))
	return u, nil
}

// Prometheus renders the Wallet's counters. No person, pass or code is
// named.
func (w *Wallet) Prometheus() string {
	if w == nil {
		return ""
	}
	return w.metrics.prometheus()
}

// walletFailures counts wrong codes per scanner and pass in fixed windows.
type walletFailures struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]walletFailure
}

type walletFailure struct {
	count   int
	resetAt time.Time
}

func (f *walletFailures) blocked(key string, now time.Time) (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[key]
	if !ok || !now.Before(entry.resetAt) {
		return 0, false
	}
	if entry.count < f.limit {
		return 0, false
	}
	return entry.resetAt.Sub(now), true
}

func (f *walletFailures) fail(key string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) > 4096 {
		for id, entry := range f.entries {
			if !now.Before(entry.resetAt) {
				delete(f.entries, id)
			}
		}
	}
	entry, ok := f.entries[key]
	if !ok || !now.Before(entry.resetAt) {
		entry = walletFailure{resetAt: now.Add(f.window)}
	}
	entry.count++
	f.entries[key] = entry
}

// walletCounter is a counter per label value, the values fixed up front.
type walletCounter struct {
	name, label string
	order       []string
	values      map[string]*atomic.Uint64
}

func newWalletCounter(name, label string, values ...string) *walletCounter {
	c := &walletCounter{name: name, label: label, order: values, values: map[string]*atomic.Uint64{}}
	for _, v := range values {
		c.values[v] = new(atomic.Uint64)
	}
	return c
}

func (c *walletCounter) add(value string) {
	if counter, ok := c.values[value]; ok {
		counter.Add(1)
	}
}

func (c *walletCounter) render(out *strings.Builder) {
	out.WriteString("# TYPE " + c.name + " counter\n")
	for _, v := range c.order {
		labels := ""
		for i, part := range strings.Split(v, ",") {
			names := strings.Split(c.label, ",")
			if i > 0 {
				labels += ","
			}
			labels += names[i] + `="` + part + `"`
		}
		out.WriteString(c.name + "{" + labels + "} " + strconv.FormatUint(c.values[v].Load(), 10) + "\n")
	}
}

type walletMetrics struct {
	enabled     bool
	codes       *walletCounter
	skew        *walletCounter
	links       *walletCounter
	withdrawals *walletCounter
}

func newWalletMetrics(enabled bool) *walletMetrics {
	return &walletMetrics{
		enabled: enabled,
		codes: newWalletCounter("skylab_skypass_google_wallet_codes_total", "outcome",
			"accepted", "malformed", "unknown_pass", "revoked", "wrong_code", "used", "inactive_account", "rate_limited", "off"),
		// Which step an accepted code was of, against core's clock: many
		// -1/+1 is a phone or server clock that is off.
		skew:  newWalletCounter("skylab_skypass_google_wallet_code_skew_total", "step", "-1", "0", "1"),
		links: newWalletCounter("skylab_skypass_google_wallet_links_total", "outcome", "issued", "failed", "off"),
		withdrawals: newWalletCounter("skylab_skypass_google_wallet_withdrawals_total", "reason,outcome",
			"member,done", "member,failed", "erasure,done", "erasure,failed", "retry,done", "retry,failed",
			"link_race,done", "link_race,failed"),
	}
}

func (m *walletMetrics) prometheus() string {
	var out strings.Builder
	out.WriteString("# TYPE skylab_skypass_google_wallet_enabled gauge\n")
	if m.enabled {
		out.WriteString("skylab_skypass_google_wallet_enabled 1\n")
	} else {
		out.WriteString("skylab_skypass_google_wallet_enabled 0\n")
	}
	m.codes.render(&out)
	m.skew.render(&out)
	m.links.render(&out)
	m.withdrawals.render(&out)
	return out.String()
}
