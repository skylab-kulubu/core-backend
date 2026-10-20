package commpref

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

func TestTrackingModeFromEnv(t *testing.T) {
	for raw, want := range map[string]TrackingMode{"": TrackingOptOut, "  ": TrackingOptOut, "opt_out": TrackingOptOut, "opt_in": TrackingOptIn, " opt_in ": TrackingOptIn} {
		got, err := TrackingModeFromEnv(func(string) string { return raw })
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"off", "on", "OPT_OUT", "optout", "opt-in"} {
		if _, err := TrackingModeFromEnv(func(string) string { return raw }); err == nil || !strings.Contains(err.Error(), TrackingModeEnv) || strings.Contains(err.Error(), raw) {
			t.Fatalf("%q accepted or echoed: %v", raw, err)
		}
	}
	if (*Service)(nil).TrackingMode() != TrackingOptOut || NewService(Config{}, nil, nil, nil).TrackingMode() != TrackingOptOut {
		t.Fatal("an unset mode is not opt_out")
	}
}

// The resolver's table: an active Member is tracked by their row or, without
// one, by the mode; the first rule that fails names the reason (inactive,
// then not a Member, then the preference). It needs no link key.
func TestResolveTracking(t *testing.T) {
	f := newAudienceFixture(t, false)
	f.choose("air", Tracking(), false)
	f.choose("drone", Tracking(), true)
	f.choose("disabled", Tracking(), true)
	f.choose("outsider", Tracking(), true)
	asked := []string{"front", "air", "drone", "disabled", "erased", "outsider", "service", "weblabYK"}
	stranger := uuid.MustParse("00000000-0000-4000-8000-999999999999")
	subjects := []uuid.UUID{}
	for _, name := range asked {
		subjects = append(subjects, f.p[name])
	}
	subjects = append(subjects, stranger)

	for _, c := range []struct {
		mode TrackingMode
		want map[string]TrackingReason // "" allowed
	}{
		{TrackingOptOut, map[string]TrackingReason{
			"front": "", "air": TrackingPreferenceOff, "drone": "", "disabled": TrackingInactive, "erased": TrackingInactive,
			"outsider": TrackingNotMember, "service": TrackingNotMember, "weblabYK": "",
		}},
		{TrackingOptIn, map[string]TrackingReason{
			"front": TrackingPreferenceOff, "air": TrackingPreferenceOff, "drone": "", "disabled": TrackingInactive, "erased": TrackingInactive,
			"outsider": TrackingNotMember, "service": TrackingNotMember, "weblabYK": TrackingPreferenceOff,
		}},
	} {
		f.prefs.SetTrackingMode(c.mode)
		// Without the key too: tracking needs none.
		f.prefs.config = Config{}
		got, err := f.audience.ResolveTracking(context.Background(), subjects)
		if err != nil || len(got) != len(subjects) {
			t.Fatalf("%s: %v %v", c.mode, got, err)
		}
		for i, d := range got {
			if d.Subject != subjects[i] {
				t.Fatalf("%s: order %d %v", c.mode, i, d)
			}
			if i == len(asked) {
				if d.Allowed || d.Reason != TrackingNotMember {
					t.Fatalf("%s stranger: %+v", c.mode, d)
				}
				continue
			}
			want := c.want[asked[i]]
			if d.Allowed != (want == "") || d.Reason != want {
				t.Fatalf("%s %s: %+v, want %q", c.mode, asked[i], d, want)
			}
		}
	}

	for name, subjects := range map[string][]uuid.UUID{
		"twice":    {f.p["front"], f.p["front"]},
		"nil":      {uuid.Nil},
		"too many": make([]uuid.UUID, MaxTrackingSubjects+1),
	} {
		if name == "too many" {
			for i := range subjects {
				subjects[i] = uuid.New()
			}
		}
		if _, err := f.audience.ResolveTracking(context.Background(), subjects); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got, err := f.audience.ResolveTracking(context.Background(), nil); err != nil || len(got) != 0 {
		t.Fatalf("none asked: %v %v", got, err)
	}
	f.clock = f.clock.Add(time.Hour)
	f.kc.fail = errors.New("keycloak down")
	if _, err := f.audience.ResolveTracking(context.Background(), []uuid.UUID{f.p["front"]}); !errors.Is(err, ErrTeamsUnavailable) {
		t.Fatalf("keycloak down: %v", err)
	}
	if (&Audience{}).TrackingAvailable() || (*Audience)(nil).TrackingAvailable() {
		t.Fatal("an audience without a directory answers")
	}
}

// The Account center's switch: the mode's default without a row, the
// person's choice with one, effective only for a Member. Turning it off
// queues one withdrawal and wakes the worker; the audit trail keeps every
// change of value, not a repeat. The switch is never forgotten (null) and
// only on e-mail.
func TestTrackingSwitch(t *testing.T) {
	f := newFixture(t)
	if got := f.mine().Tracking; got != (TrackingState{Enabled: true, Effective: true, Mode: TrackingOptOut}) {
		t.Fatalf("default %+v", got)
	}
	f.svc.SetTrackingMode(TrackingOptIn)
	if got := f.mine().Tracking; got != (TrackingState{Enabled: false, Effective: false, Mode: TrackingOptIn}) {
		t.Fatalf("opt_in default %+v", got)
	}
	f.svc.SetTrackingMode(TrackingOptOut)
	woken := 0
	f.svc.OnTrackingWithdrawn(func() { woken++ })

	screen, err := f.update(set(Tracking(), false))
	if err != nil || screen.Tracking != (TrackingState{Enabled: false, Effective: false, Mode: TrackingOptOut}) || woken != 1 {
		t.Fatalf("off: %+v %v woken=%d", screen.Tracking, err, woken)
	}
	if w := f.store.Withdrawals(); len(w) != 1 || w[0].Subject != f.person || w[0].Attempts != 0 {
		t.Fatalf("withdrawals %+v", w)
	}
	if _, err := f.update(set(Tracking(), false)); err != nil || woken != 2 || len(f.store.Withdrawals()) != 1 {
		t.Fatalf("off again: %v woken=%d %+v", err, woken, f.store.Withdrawals())
	}
	screen, err = f.update(set(Tracking(), true), set(Club(KindNews), false))
	if err != nil || !screen.Tracking.Enabled || !screen.Tracking.Effective || woken != 2 || screen.Club.Haber[ChannelEmail].Enabled {
		t.Fatalf("on: %+v %v woken=%d", screen.Tracking, err, woken)
	}
	audit, _ := f.store.TrackingChanges(context.Background(), f.person)
	if len(audit) != 2 || audit[0].Enabled || audit[0].Previous != nil || !audit[1].Enabled || audit[1].Previous == nil || *audit[1].Previous ||
		audit[0].Source != SourceAccountCenter {
		t.Fatalf("audit %+v", audit)
	}
	// The tracking row is no News category: the screen's lists ignore it.
	if len(screen.Following) != 0 || !screen.Master[ChannelEmail].Enabled {
		t.Fatalf("screen %+v", screen)
	}

	for name, change := range map[string]Change{
		"forget":     forget(Tracking()),
		"on a group": {Channel: ChannelEmail, Category: Category{Scope: ScopeTracking, GroupID: "g-front"}, Enabled: func() *bool { v := false; return &v }()},
		"no channel": {Category: Tracking(), Enabled: func() *bool { v := false; return &v }()},
	} {
		if _, err := f.update(change); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := f.update(set(Tracking(), false), set(Tracking(), true)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("twice: %v", err)
	}
	if _, err := ParseCategory("tracking"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tracking is a News category: %v", err)
	}

	// A person being erased gets no row, so no withdrawal either.
	f.store.MarkInactive(f.person)
	before := len(f.store.Withdrawals())
	if _, err := f.update(set(Tracking(), false)); !errors.Is(err, ErrSubjectInactive) {
		t.Fatalf("inactive: %v", err)
	}
	if n, err := f.store.EraseSubject(context.Background(), f.person); err != nil || n == 0 {
		t.Fatalf("erase %d %v", n, err)
	}
	if audit, _ := f.store.TrackingChanges(context.Background(), f.person); len(audit) != 0 || len(f.store.Withdrawals()) > before {
		t.Fatalf("after erasure: audit %+v withdrawals %+v", audit, f.store.Withdrawals())
	}
	if len(f.store.Withdrawals()) != 0 {
		t.Fatalf("erasure kept the withdrawal: %+v", f.store.Withdrawals())
	}
}

// A non-Member may still object; the switch is never effective for them.
func TestTrackingSwitchForANonMember(t *testing.T) {
	f := newFixture(t)
	if err := f.dir.RemoveMember(context.Background(), "g-front", f.person); err != nil {
		t.Fatal(err)
	}
	f.svc.cached = nil
	if got := f.mine().Tracking; got != (TrackingState{Enabled: true, Effective: false, Mode: TrackingOptOut}) {
		t.Fatalf("non-member %+v", got)
	}
	if screen, err := f.update(set(Tracking(), false)); err != nil || screen.Tracking.Enabled {
		t.Fatalf("non-member off: %+v %v", screen.Tracking, err)
	}
}

// fakeSender answers from a script, one result per call.
type fakeSender struct {
	mu      sync.Mutex
	results []WithdrawalResult
	calls   []uuid.UUID
}

func (s *fakeSender) Withdraw(_ context.Context, subject uuid.UUID) WithdrawalResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, subject)
	if len(s.results) == 0 {
		return WithdrawalResult{Outcome: WithdrawalSent, Status: http.StatusNoContent}
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r
}

// The worker sends each waiting withdrawal until SkyMail confirms it: a
// failure waits its backoff (Retry-After kept between 30 s and 15 min; 403
// and 404 the longest), a 400 is dropped, nothing is ever given up on.
func TestWithdrawalsRetryUntilSent(t *testing.T) {
	store := NewMemoryStore()
	person := uuid.New()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	off := false
	if err := store.Apply(context.Background(), person, []Change{{Channel: ChannelEmail, Category: Tracking(), Enabled: &off}}, SourceAccountCenter, now); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{results: []WithdrawalResult{
		{Outcome: WithdrawalRetry, Status: http.StatusServiceUnavailable, RetryAfter: 2 * time.Second},
		{Outcome: WithdrawalForbidden, Status: http.StatusForbidden},
		{Outcome: WithdrawalNotFound, Status: http.StatusNotFound},
		{Outcome: WithdrawalRetry, Status: http.StatusUnauthorized},
		{Outcome: WithdrawalSent, Status: http.StatusNoContent},
	}}
	w := NewWithdrawals(store, sender)
	w.SetClock(func() time.Time { return now })
	var logged []string
	w.logf = func(format string, args ...any) { logged = append(logged, format) }
	waits := []time.Duration{withdrawalRetryFirst, withdrawalRetryMax, withdrawalRetryMax, 8 * withdrawalRetryFirst}
	for i, wait := range waits {
		report, err := w.Pass(context.Background())
		if err != nil || report.Sent != 0 {
			t.Fatalf("pass %d: %+v %v", i, report, err)
		}
		pending := store.Withdrawals()
		if len(pending) != 1 || pending[0].Attempts != i+1 || !pending[0].NextAttemptAt.Equal(now.Add(wait)) || !pending[0].ClaimedAt.IsZero() {
			t.Fatalf("pass %d: %+v, want due in %s", i, pending, wait)
		}
		// Not due yet: nothing is sent.
		if report, _ := w.Pass(context.Background()); report.any() {
			t.Fatalf("pass %d sent early: %+v", i, report)
		}
		now = now.Add(wait)
	}
	report, err := w.Pass(context.Background())
	if err != nil || report.Sent != 1 || len(store.Withdrawals()) != 0 || len(sender.calls) != 5 || sender.calls[4] != person {
		t.Fatalf("sent: %+v %v %+v", report, err, sender.calls)
	}
	metrics := w.Prometheus()
	for _, line := range []string{
		`skylab_tracking_withdrawal_enabled 1`, `skylab_tracking_withdrawal_total{outcome="sent"} 1`,
		`skylab_tracking_withdrawal_total{outcome="retry"} 2`, `skylab_tracking_withdrawal_total{outcome="forbidden"} 1`,
		`skylab_tracking_withdrawal_total{outcome="not_found"} 1`, `skylab_tracking_withdrawal_backlog 0`,
	} {
		if !strings.Contains(metrics, line) {
			t.Fatalf("metrics lack %s:\n%s", line, metrics)
		}
	}
	if strings.Contains(metrics, person.String()) || !strings.Contains(strings.Join(logged, "\n"), "tracking_withdrawal_forbidden") {
		t.Fatalf("metrics or logs: %s %v", metrics, logged)
	}

	// A 400 is dropped (and alarmed), not tried again.
	if err := store.Apply(context.Background(), person, []Change{{Channel: ChannelEmail, Category: Tracking(), Enabled: &off}}, SourceAccountCenter, now); err != nil {
		t.Fatal(err)
	}
	sender.results = []WithdrawalResult{{Outcome: WithdrawalRejected, Status: http.StatusBadRequest}}
	if report, err := w.Pass(context.Background()); err != nil || report.Rejected != 1 || len(store.Withdrawals()) != 0 {
		t.Fatalf("rejected: %+v %v %+v", report, err, store.Withdrawals())
	}

	// Without a sender nothing is claimed, the backlog still counts.
	if err := store.Apply(context.Background(), uuid.New(), []Change{{Channel: ChannelEmail, Category: Tracking(), Enabled: &off}}, SourceAccountCenter, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	idle := NewWithdrawals(store, nil)
	idle.SetClock(func() time.Time { return now })
	if report, err := idle.Pass(context.Background()); err != nil || report.any() || idle.Sending() {
		t.Fatalf("idle: %+v %v", report, err)
	}
	if m := idle.Prometheus(); !strings.Contains(m, "skylab_tracking_withdrawal_enabled 0") || !strings.Contains(m, "skylab_tracking_withdrawal_backlog 1") ||
		!strings.Contains(m, "skylab_tracking_withdrawal_oldest_age_seconds 60") {
		t.Fatalf("idle metrics:\n%s", m)
	}
}

// A newer request while a send is in flight is not lost: the late answer of
// the older claim neither deletes nor delays it.
func TestWithdrawalRequestDuringASendIsKept(t *testing.T) {
	store := NewMemoryStore()
	person := uuid.New()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	off := false
	apply := func(at time.Time) {
		if err := store.Apply(context.Background(), person, []Change{{Channel: ChannelEmail, Category: Tracking(), Enabled: &off}}, SourceAccountCenter, at); err != nil {
			t.Fatal(err)
		}
	}
	apply(now)
	claimed, err := store.ClaimWithdrawals(context.Background(), now, withdrawalLease, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim %v %v", claimed, err)
	}
	apply(now.Add(time.Second))
	if err := store.CompleteWithdrawal(context.Background(), claimed[0]); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryWithdrawal(context.Background(), claimed[0], now.Add(time.Hour), 503); err != nil {
		t.Fatal(err)
	}
	pending := store.Withdrawals()
	if len(pending) != 1 || !pending[0].NextAttemptAt.Equal(now.Add(time.Second)) || pending[0].Attempts != 0 {
		t.Fatalf("pending %+v", pending)
	}
}

type staticTokens struct {
	mu          sync.Mutex
	token       string
	invalidated int
	err         error
}

func (s *staticTokens) Token(context.Context) (string, error) { return s.token, s.err }
func (s *staticTokens) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated++
}

// The HTTP sender: PUT …/internal/v1/tracking-withdrawals/{sub} with the
// token, no body, no forwarding header; each answer classified; the token
// forgotten on 401 and 403; a redirect never followed.
func TestSkyMailWithdrawals(t *testing.T) {
	subject := uuid.MustParse("7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10")
	var status int
	var retryAfterHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/skymail/internal/v1/tracking-withdrawals/"+subject.String() ||
			r.Header.Get("Authorization") != "Bearer t0k" || r.ContentLength > 0 {
			t.Errorf("request %s %s %q len=%d", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.ContentLength)
		}
		for name := range r.Header {
			if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-forwarded-") || lower == "forwarded" || lower == "x-real-ip" {
				t.Errorf("forwarding header %s", name)
			}
		}
		if retryAfterHeader != "" {
			w.Header().Set("Retry-After", retryAfterHeader)
		}
		if status == http.StatusFound {
			w.Header().Set("Location", "https://example.test/elsewhere")
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	tokens := &staticTokens{token: "t0k"}
	sender := &SkyMailWithdrawals{BaseURL: srv.URL + "/skymail", Tokens: tokens}
	for _, c := range []struct {
		status      int
		retryAfter  string
		outcome     WithdrawalOutcome
		wait        time.Duration
		invalidated int
	}{
		{http.StatusNoContent, "", WithdrawalSent, 0, 0},
		{http.StatusServiceUnavailable, "30", WithdrawalRetry, 30 * time.Second, 0},
		{http.StatusTooManyRequests, "", WithdrawalRetry, 0, 0},
		{http.StatusUnauthorized, "", WithdrawalRetry, 0, 1},
		{http.StatusForbidden, "", WithdrawalForbidden, 0, 2},
		{http.StatusNotFound, "", WithdrawalNotFound, 0, 2},
		{http.StatusBadRequest, "", WithdrawalRejected, 0, 2},
		{http.StatusFound, "", WithdrawalRetry, 0, 2},
	} {
		status, retryAfterHeader = c.status, c.retryAfter
		got := sender.Withdraw(context.Background(), subject)
		if got.Outcome != c.outcome || got.Status != c.status || got.RetryAfter != c.wait || tokens.invalidated != c.invalidated ||
			strings.Contains(got.Reason, subject.String()) || strings.Contains(got.Reason, srv.URL) {
			t.Fatalf("%d: %+v invalidated=%d", c.status, got, tokens.invalidated)
		}
	}
	tokens.err = errors.New("keycloak down")
	if got := sender.Withdraw(context.Background(), subject); got.Outcome != WithdrawalRetry || got.Status != 0 {
		t.Fatalf("no token: %+v", got)
	}
	tokens.err = nil
	srv.Close()
	if got := sender.Withdraw(context.Background(), subject); got.Outcome != WithdrawalRetry || got.Status != 0 || strings.Contains(got.Reason, "127.0.0.1") {
		t.Fatalf("down: %+v", got)
	}
}

func TestWithdrawalURLFromEnv(t *testing.T) {
	env := func(values map[string]string) func(string) string { return func(k string) string { return values[k] } }
	if base, from, err := WithdrawalURLFromEnv(env(nil)); base != "" || from != "" || err != nil {
		t.Fatalf("unset: %q %q %v", base, from, err)
	}
	if base, from, err := WithdrawalURLFromEnv(env(map[string]string{withdrawalURLFallbackEnv: "http://skymail-backend-x:8080/"})); base != "http://skymail-backend-x:8080" || from != withdrawalURLFallbackEnv || err != nil {
		t.Fatalf("fallback: %q %q %v", base, from, err)
	}
	if base, from, err := WithdrawalURLFromEnv(env(map[string]string{WithdrawalURLEnv: "http://a:1", withdrawalURLFallbackEnv: "http://b:2"})); base != "http://a:1" || from != WithdrawalURLEnv || err != nil {
		t.Fatalf("own: %q %q %v", base, from, err)
	}
	for _, bad := range []string{"skymail:8080", "http://u:p@a", "http://a/?q=1", "ftp://a"} {
		if _, _, err := WithdrawalURLFromEnv(env(map[string]string{WithdrawalURLEnv: bad})); err == nil || strings.Contains(err.Error(), bad) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	lines := strings.Join(TrackingStartupLines(TrackingOptOut, false, ""), "\n")
	if !strings.Contains(lines, "mode opt_out") || !strings.Contains(lines, "WARNING") {
		t.Fatalf("startup lines %s", lines)
	}
}

// An objection is written while Keycloak is down: a tracking-only change
// reads no team, writes the row and the withdrawal, and answers a partial
// screen. A change with News items still needs the tree.
func TestTrackingSwitchWhileKeycloakIsDown(t *testing.T) {
	f := newFixture(t)
	svc := NewService(f.svc.Config(), f.store, failingDirectory{}, nil)
	screen, err := svc.Update(context.Background(), f.person, []Change{set(Tracking(), false)})
	if err != nil || !screen.Partial || screen.Tracking != (TrackingState{Enabled: false, Effective: false, Mode: TrackingOptOut}) ||
		screen.Teams == nil || screen.Followable == nil || len(screen.Channels) != 1 {
		t.Fatalf("partial %+v %v", screen, err)
	}
	if w := f.store.Withdrawals(); len(w) != 1 || w[0].Subject != f.person {
		t.Fatalf("withdrawals %+v", w)
	}
	if _, err := svc.Update(context.Background(), f.person, []Change{set(Tracking(), true), set(Club(KindNews), false)}); !errors.Is(err, ErrTeamsUnavailable) {
		t.Fatalf("with an item: %v", err)
	}
	// With Keycloak answering, a tracking-only change gets the full screen.
	if screen, err := f.update(set(Tracking(), true)); err != nil || screen.Partial || !screen.Tracking.Effective || len(screen.Teams) != 1 {
		t.Fatalf("full %+v %v", screen, err)
	}
}

// MaxChanges bounds the News items; the tracking switch comes on top.
func TestTrackingSwitchOnTopOfMaxChanges(t *testing.T) {
	f := newFixture(t)
	items := make([]Change, 0, MaxChanges+1)
	for i := range MaxChanges {
		items = append(items, forget(Team(fmt.Sprintf("g-gone-%d", i), KindAnnouncement)))
	}
	if _, err := f.update(append(slices.Clone(items), set(Tracking(), false))...); err != nil {
		t.Fatalf("%d items and tracking: %v", MaxChanges, err)
	}
	items = append(items, forget(Team("g-gone-last", KindAnnouncement)))
	if _, err := f.update(items...); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%d items: %v", len(items), err)
	}
}

// The resolver's role is a known service account role (never an
// unclassified one in the role log).
func TestTrackingResolveRoleIsAKnownServiceRole(t *testing.T) {
	if !slices.Contains(authz.AdminRoles(), authz.RoleRef{Client: "core", Role: TrackingResolveRole}) {
		t.Fatal("communication-preferences:read is not in authz.AdminRoles")
	}
}
