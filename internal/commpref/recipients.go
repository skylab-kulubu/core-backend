package commpref

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The recipient question (ADR 0072 karar 3; sky_lab_genel
// .scratch/abonelik-cikisi bilet 01; docs/news-audience.md, "Recipients"):
// SkyMail names the recipients a sender chose (a list, a Keycloak Group, a
// form's respondents) and asks, for one category, which of them are a
// person, whether the mail may go to each, and each one's unsubscribe link.
// Unlike the News audience, core picks nobody here: the sender chose the
// list, so only the person's own "off" holds a mail back (ResolveChosen).

const (
	// MaxRecipients is the most recipients one question names.
	MaxRecipients = 1000
	// maxRecipientAddress bounds an address (RFC 5321's path limit, with
	// room).
	maxRecipientAddress = 320

	// AddressDirectoryFresh is how long a read of the verified-address
	// directory answers without a new read: the first question after it
	// starts one, and is answered from the directory it has.
	AddressDirectoryFresh = 5 * time.Minute
	// AddressDirectoryMaxAge is the oldest directory a question is
	// answered from: older, the question waits for a new read and is
	// refused when that fails.
	AddressDirectoryMaxAge = 15 * time.Minute
	// addressDirectoryRetry is how long a failed read keeps a new one from
	// starting, so a Keycloak that does not answer gets one attempt in
	// that time, not one per question.
	addressDirectoryRetry = 30 * time.Second
	// addressDirectoryReadTimeout bounds one read of the whole directory.
	// It runs on its own context: a caller giving up does not stop the
	// read the others wait on.
	addressDirectoryReadTimeout = 3 * time.Minute
	// AddressDirectoryPage is how many accounts one Keycloak read asks
	// for. Chosen by measurement (docs/news-audience.md, "Recipients").
	AddressDirectoryPage = 500
	// maxAddressDirectoryRows ends a read that never ends: offset paging
	// over a realm that grows as fast as it is read.
	maxAddressDirectoryRows = 2_000_000
)

// YTUIdentityProviderEnv names the YTÜ Microsoft identity provider's alias
// (e-skylab-keycloak's ytuIdpAlias, SKY_ACCOUNT_YTU_IDP_ALIAS); unset is
// DefaultYTUIdentityProvider. A School e-mail is a person's address only
// while their account is linked to it, as on the login form.
const (
	YTUIdentityProviderEnv     = "KEYCLOAK_YTU_IDP_ALIAS"
	DefaultYTUIdentityProvider = "OBS"
)

var identityProviderAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// YTUIdentityProviderFromEnv reads YTUIdentityProviderEnv.
func YTUIdentityProviderFromEnv(getenv func(string) string) (string, error) {
	alias := strings.TrimSpace(getenv(YTUIdentityProviderEnv))
	if alias == "" {
		return DefaultYTUIdentityProvider, nil
	}
	if !identityProviderAlias.MatchString(alias) {
		return "", fmt.Errorf("%s must be an identity provider alias (letters, digits, '.', '_', '-')", YTUIdentityProviderEnv)
	}
	return alias, nil
}

// ErrAddressDirectoryUnavailable is a question the verified-address
// directory cannot answer: it was never read, or it is older than
// AddressDirectoryMaxAge and Keycloak does not answer.
var ErrAddressDirectoryUnavailable = errors.New("communication preference: address directory unavailable")

// AddressDirectory is the Keycloak reads of the verified-address
// directory: every account of the realm page by page (full
// representation), and the accounts linked to an identity provider.
type AddressDirectory interface {
	AddressHoldersPage(ctx context.Context, first, max int) (identity.AddressHolderPage, error)
	IdentityProviderLinkedPage(ctx context.Context, alias string, first, max int) (identity.LinkedPage, error)
}

// RecipientQuery is one recipient a sender chose: an address or a Keycloak
// subject, exactly one of the two.
type RecipientQuery struct {
	Email string
	Sub   uuid.UUID
}

// RecipientReason is why a mail may not go to a person.
type RecipientReason string

const (
	// RecipientInactive: disabled in Keycloak, or being erased or erased
	// in core.
	RecipientInactive RecipientReason = "inactive"
	// RecipientOptedOut: the channel's master switch is off, or the
	// person's explicit choice that governs the category is off.
	RecipientOptedOut RecipientReason = "opted_out"
	// RecipientAmbiguous: the address is a verified address of two or more
	// accounts; nobody's preference can stand for it.
	RecipientAmbiguous RecipientReason = "ambiguous"
)

// RecipientDecision is the answer for one recipient.
type RecipientDecision struct {
	// Matched is whether the recipient is a person core knows: a verified
	// address of an account, or the subject of one. A recipient that is not
	// is left to SkyMail's own record and link.
	Matched bool
	// Sub is the person's; uuid.Nil when not matched or ambiguous.
	Sub     uuid.UUID
	Allowed bool
	// Reason is empty when Allowed or not Matched.
	Reason RecipientReason
	// UnsubscribeURL turns exactly the category off for Sub; only when
	// Allowed.
	UnsubscribeURL string
}

// addressIndex is one complete read of the verified-address directory.
type addressIndex struct {
	// byAddress are the accounts each verified address (trimmed, lower
	// case) belongs to: a set, each account once, so a page read twice
	// does not make an address ambiguous.
	byAddress map[string][]uuid.UUID
	// enabled is every account read but the service accounts, with
	// Keycloak's enabled flag.
	enabled map[uuid.UUID]bool
}

// normalizeAddress is how an address is compared: trimmed and lower case,
// as the login form compares it (LoginIdentifiers).
func normalizeAddress(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

func (x *addressIndex) add(raw string, id uuid.UUID) {
	address := normalizeAddress(raw)
	if address == "" {
		return
	}
	for _, held := range x.byAddress[address] {
		if held == id {
			return
		}
	}
	x.byAddress[address] = append(x.byAddress[address], id)
}

// personalEmailProven reports whether the proof stamp sky-account writes
// reads as a moment (IdentityResource.isPersonalEmailVerified).
func personalEmailProven(stamp string) bool {
	stamp = strings.TrimSpace(stamp)
	if stamp == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, stamp)
	return err == nil
}

// noTeams is the tree of a club category's question.
var noTeams = NewTree(nil, nil)

// Recipients answers the recipient question.
type Recipients struct {
	prefs    *Service
	dir      AddressDirectory
	accounts AudienceAccounts
	ytuAlias string
	pageSize int
	now      func() time.Time

	mu       sync.Mutex
	current  *addressIndex
	readAt   time.Time
	failedAt time.Time
	inflight *addressRead
}

// addressRead is one read of the directory in flight; done closes when it
// ends, err is then its outcome.
type addressRead struct {
	done chan struct{}
	err  error
}

// NewRecipients answers over prefs' preferences and link key, reading the
// verified addresses from dir and the School e-mail's link from the
// identity provider ytuAlias.
func NewRecipients(prefs *Service, dir AddressDirectory, accounts AudienceAccounts, ytuAlias string) *Recipients {
	return &Recipients{prefs: prefs, dir: dir, accounts: accounts, ytuAlias: ytuAlias, pageSize: AddressDirectoryPage, now: time.Now}
}

// SetClock replaces the clock (tests).
func (r *Recipients) SetClock(now func() time.Time) { r.now = now }

// SetPageSize replaces the page size of a directory read (tests,
// measurements).
func (r *Recipients) SetPageSize(n int) {
	if n > 0 {
		r.pageSize = n
	}
}

// Enabled reports whether the question can be answered: the preferences
// are on (every allowed person gets a signed link).
func (r *Recipients) Enabled() bool {
	return r != nil && r.prefs.Enabled() && r.dir != nil && r.accounts != nil
}

// index is the directory to answer from. A fresh one answers; one older
// than AddressDirectoryFresh answers and starts a new read; none, or one
// older than AddressDirectoryMaxAge, waits for a read and fails with it.
func (r *Recipients) index(ctx context.Context) (*addressIndex, error) {
	now := r.now()
	r.mu.Lock()
	current, age := r.current, now.Sub(r.readAt)
	r.mu.Unlock()
	if current != nil && age < AddressDirectoryFresh {
		return current, nil
	}
	read := r.startRead(now)
	if current != nil && age < AddressDirectoryMaxAge {
		return current, nil
	}
	if read == nil {
		// A read failed a moment ago: no new one yet.
		return nil, ErrAddressDirectoryUnavailable
	}
	select {
	case <-read.done:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrAddressDirectoryUnavailable, ctx.Err())
	}
	if read.err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAddressDirectoryUnavailable, read.err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current, nil
}

// startRead is the read in flight, or a new one; nil when the last read
// failed less than addressDirectoryRetry ago. One read runs at a time.
func (r *Recipients) startRead(now time.Time) *addressRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight != nil {
		return r.inflight
	}
	if !r.failedAt.IsZero() && now.Sub(r.failedAt) < addressDirectoryRetry {
		return nil
	}
	read := &addressRead{done: make(chan struct{})}
	r.inflight = read
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), addressDirectoryReadTimeout)
		index, err := r.readIndex(ctx)
		cancel()
		r.mu.Lock()
		if err == nil {
			// Only a complete read replaces the directory, at once; a
			// failed one leaves the old one as it was.
			r.current, r.readAt, r.failedAt = index, now, time.Time{}
		} else {
			r.failedAt = r.now()
		}
		read.err = err
		r.inflight = nil
		r.mu.Unlock()
		close(read.done)
	}()
	return read
}

// readIndex reads the whole directory: the accounts linked to the YTÜ
// identity provider, then every account page by page. Either read failing
// fails it.
func (r *Recipients) readIndex(ctx context.Context) (*addressIndex, error) {
	linked := map[uuid.UUID]bool{}
	for first := 0; ; first += r.pageSize {
		if first > maxAddressDirectoryRows {
			return nil, errors.New("identity provider link read does not end")
		}
		page, err := r.dir.IdentityProviderLinkedPage(ctx, r.ytuAlias, first, r.pageSize)
		if err != nil {
			return nil, err
		}
		for _, id := range page.IDs {
			linked[id] = true
		}
		if page.Rows < r.pageSize {
			break
		}
	}
	index := &addressIndex{byAddress: map[string][]uuid.UUID{}, enabled: map[uuid.UUID]bool{}}
	for first := 0; ; first += r.pageSize {
		if first > maxAddressDirectoryRows {
			return nil, errors.New("address directory read does not end")
		}
		page, err := r.dir.AddressHoldersPage(ctx, first, r.pageSize)
		if err != nil {
			return nil, err
		}
		for _, h := range page.Holders {
			// A client's service account is no person: never matched.
			if h.ServiceAccount || h.ID == uuid.Nil {
				continue
			}
			index.enabled[h.ID] = h.Enabled
			// The identifiers the login form takes (ADR-0044): the
			// verified Primary e-mail, the School e-mail of an account
			// linked to YTÜ, the Personal e-mail with its proof stamp.
			if h.EmailVerified {
				index.add(h.Email, h.ID)
			}
			if linked[h.ID] {
				index.add(h.SchoolEmail, h.ID)
			}
			if personalEmailProven(h.PersonalEmailVerifiedAt) {
				index.add(h.PersonalEmail, h.ID)
			}
		}
		if page.Rows < r.pageSize {
			return index, nil
		}
	}
}

// CheckRecipients checks a question before anything is read: channel email,
// a club or team category (not the master switch), 1 to MaxRecipients
// recipients, each an address or a subject, none twice (addresses compared
// trimmed and lower case).
func CheckRecipients(ch Channel, cat Category, queries []RecipientQuery) error {
	if ch != ChannelEmail || !cat.Valid() || (cat.Scope != ScopeClub && cat.Scope != ScopeTeam) {
		return ErrInvalid
	}
	if len(queries) == 0 || len(queries) > MaxRecipients {
		return ErrInvalid
	}
	addresses := make(map[string]bool, len(queries))
	subs := make(map[uuid.UUID]bool, len(queries))
	for _, q := range queries {
		switch {
		case q.Email != "" && q.Sub == uuid.Nil:
			address := normalizeAddress(q.Email)
			if address == "" || len(address) > maxRecipientAddress || !strings.Contains(address, "@") || addresses[address] {
				return ErrInvalid
			}
			addresses[address] = true
		case q.Email == "" && q.Sub != uuid.Nil:
			if subs[q.Sub] {
				return ErrInvalid
			}
			subs[q.Sub] = true
		default:
			return ErrInvalid
		}
	}
	return nil
}

// Resolve answers each recipient of queries, in order, for category cat on
// channel ch (ADR 0072 karar 3). A recipient is matched by a verified
// address in the directory or by a subject it holds; then the first of
// these that holds is the answer: inactive (disabled in Keycloak, being
// erased or erased in core), opted out (the master switch, or the
// governing explicit choice, off), else allowed with the person's link for
// cat. The preferences and core's account state are read now; the
// directory may be up to AddressDirectoryFresh old (AddressDirectoryMaxAge
// while Keycloak does not answer).
func (r *Recipients) Resolve(ctx context.Context, ch Channel, cat Category, queries []RecipientQuery) ([]RecipientDecision, error) {
	if !r.Enabled() {
		return nil, ErrDisabled
	}
	if err := CheckRecipients(ch, cat, queries); err != nil {
		return nil, err
	}
	// A club category reads no team: no tree is asked for.
	tree := noTeams
	if cat.Scope == ScopeTeam {
		var err error
		if tree, err = r.prefs.tree(ctx); err != nil {
			return nil, err
		}
		// A Group teammail.TeamOf calls no team's own is no category. A
		// hidden team is a team: its own mail is a sender's to send.
		if _, ok := tree.teams[cat.GroupID]; !ok {
			return nil, ErrUnknownTeam
		}
	}
	index, err := r.index(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]RecipientDecision, len(queries))
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for i, q := range queries {
		var holders []uuid.UUID
		if q.Sub != uuid.Nil {
			if _, ok := index.enabled[q.Sub]; ok {
				holders = []uuid.UUID{q.Sub}
			}
		} else {
			holders = index.byAddress[normalizeAddress(q.Email)]
		}
		switch len(holders) {
		case 0:
			continue
		case 1:
			out[i] = RecipientDecision{Matched: true, Sub: holders[0]}
			if !seen[holders[0]] {
				seen[holders[0]] = true
				ids = append(ids, holders[0])
			}
		default:
			out[i] = RecipientDecision{Matched: true, Reason: RecipientAmbiguous}
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	accounts, err := r.accounts.Accounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	settings, err := r.prefs.store.ListPeople(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		d := &out[i]
		if !d.Matched || d.Sub == uuid.Nil {
			continue
		}
		account, stored := accounts[d.Sub]
		switch {
		case !index.enabled[d.Sub] || d.Sub == user.DeletedSubject || (stored && account.Blocked):
			d.Reason = RecipientInactive
			continue
		case !tree.ResolveChosen(ExplicitOf(settings[d.Sub], ch), cat).Effective:
			d.Reason = RecipientOptedOut
			continue
		}
		link, err := r.prefs.UnsubscribeURL(d.Sub, ch, []Category{cat})
		if err != nil {
			return nil, err
		}
		d.Allowed, d.UnsubscribeURL = true, link
	}
	return out, nil
}
