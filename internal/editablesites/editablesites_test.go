package editablesites_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/editablesites"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// sites is a configuration like production's: the main site, arge and the
// event sites with a CMS tenant.
var sites = []editablesites.Site{
	{ClientID: "frontend-main", Name: "Ana site", URL: "https://yildizskylab.com"},
	{ClientID: "frontend-arge", Name: "Ar-Ge", URL: "https://arge.yildizskylab.com"},
	{ClientID: "frontend-artlab", Name: "ARTLAB", URL: "https://artlab.yildizskylab.com"},
	{ClientID: "frontend-yildizjam", Name: "YıldızJam", URL: "https://yildizjam.yildizskylab.com"},
	{ClientID: "frontend-skydays", Name: "SkyDays", URL: "https://skydays.yildizskylab.com"},
}

// realm answers a person's effective client roles the way Keycloak does for
// the Site editor rule: cms:access is granted to Groups only, and a member of
// a Group holds what the Groups above it hold.
type realm struct {
	grants  map[string][]string    // group path → Site clients where it holds cms:access
	members map[uuid.UUID][]string // person → direct Group paths
	clients []string               // the clients the realm has
	fail    map[string]error       // client → failure
	delay   time.Duration
	panics  bool

	calls atomic.Int64
}

func (r *realm) EffectiveClientRoles(ctx context.Context, userID uuid.UUID, clientID string) ([]string, error) {
	r.calls.Add(1)
	if r.panics {
		panic("boom")
	}
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := r.fail[clientID]; err != nil {
		return nil, err
	}
	if !slices.Contains(r.clients, clientID) {
		return nil, identity.ErrNotFound
	}
	// Every site client also gives its own service account content:read;
	// only cms:access makes an editor.
	roles := []string{}
	for _, path := range r.members[userID] {
		for p := path; p != ""; p = p[:strings.LastIndex(p, "/")] {
			if slices.Contains(r.grants[p], clientID) {
				return []string{"cms:access", "content:read", "content:write"}, nil
			}
		}
	}
	return roles, nil
}

var (
	admin  = uuid.MustParse("00000000-0000-4000-8000-00000000000a")
	leader = uuid.MustParse("00000000-0000-4000-8000-00000000000b")
	member = uuid.MustParse("00000000-0000-4000-8000-00000000000c")
	reader = uuid.MustParse("00000000-0000-4000-8000-00000000000d")
)

// newRealm is production's grants (ADR-0056, CONTEXT "Site editor"):
// Privileged Groups edit every site, Leader groups edit the main site and
// arge, and AIRLAB's Leaders (ARTLAB's Owner team) also edit ARTLAB.
// frontend-skydays does not exist yet.
func newRealm() *realm {
	all := []string{"frontend-main", "frontend-arge", "frontend-artlab", "frontend-yildizjam", "frontend-skydays"}
	return &realm{
		grants: map[string][]string{
			"/ADMIN":                            all,
			"/UYELER/YK":                        all,
			"/UYELER/ARGE/AIRLAB/LIDERLER":      {"frontend-main", "frontend-arge", "frontend-artlab"},
			"/UYELER/ARGE/GAMELAB/LIDERLER":     {"frontend-main", "frontend-arge", "frontend-yildizjam"},
			"/UYELER/ORGANIZASYON/ARTLAB/LIDER": {"frontend-artlab"},
		},
		members: map[uuid.UUID][]string{
			admin:  {"/ADMIN", "/UYELER"},
			leader: {"/UYELER/ARGE/AIRLAB/LIDERLER", "/UYELER/ARGE/AIRLAB"},
			member: {"/UYELER/ARGE/AIRLAB", "/UYELER/ORGANIZASYON/ARTLAB"},
		},
		clients: all[:4],
	}
}

func clientIDs(list []editablesites.Site) []string {
	out := []string{}
	for _, s := range list {
		out = append(out, s.ClientID)
	}
	return out
}

// The list is the configured sites where the person holds cms:access, in
// the configuration's order, with their name and address: every existing
// site for a Privileged member, the sites a Leader group grants for a
// Leader, none for a plain member or for someone in no Group.
func TestForListsTheSitesWhereThePersonHoldsCMSAccess(t *testing.T) {
	t.Parallel()

	s := editablesites.New(sites, newRealm(), editablesites.Options{})
	for _, tc := range []struct {
		name string
		user uuid.UUID
		want []string
	}{
		{"privileged", admin, []string{"frontend-main", "frontend-arge", "frontend-artlab", "frontend-yildizjam"}},
		{"site leader", leader, []string{"frontend-main", "frontend-arge", "frontend-artlab"}},
		{"member", member, []string{}},
		{"no groups", reader, []string{}},
	} {
		got, cached, err := s.For(context.Background(), tc.user)
		if err != nil || cached {
			t.Fatalf("%s: cached %v, error %v", tc.name, cached, err)
		}
		if got == nil || !slices.Equal(clientIDs(got), tc.want) {
			t.Fatalf("%s: sites = %v, want %v", tc.name, clientIDs(got), tc.want)
		}
	}
	got, _, _ := s.For(context.Background(), leader)
	if got[2] != sites[2] {
		t.Fatalf("site = %+v, want %+v", got[2], sites[2])
	}
}

// One answer is kept a minute per person: the next request in that minute
// asks Keycloak nothing, a request after it asks again.
func TestForKeepsTheAnswerAMinute(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := newRealm()
	s := editablesites.New(sites, r, editablesites.Options{Now: func() time.Time { return now }})

	if _, cached, _ := s.For(context.Background(), leader); cached {
		t.Fatal("first answer came from the cache")
	}
	calls := r.calls.Load()
	if calls != int64(len(sites)) {
		t.Fatalf("calls = %d, want one per site", calls)
	}
	now = now.Add(59 * time.Second)
	got, cached, err := s.For(context.Background(), leader)
	if !cached || err != nil || len(got) != 3 || r.calls.Load() != calls {
		t.Fatalf("within the minute: cached %v, %v, %v, calls %d", cached, clientIDs(got), err, r.calls.Load())
	}
	// The answer is the caller's own copy.
	got[0].Name = "changed"
	if again, _, _ := s.For(context.Background(), leader); again[0].Name != "Ana site" {
		t.Fatal("a caller changed the cached answer")
	}
	now = now.Add(2 * time.Second)
	if _, cached, _ := s.For(context.Background(), leader); cached || r.calls.Load() != 2*calls {
		t.Fatalf("after the minute: cached %v, calls %d", cached, r.calls.Load())
	}
}

// When Keycloak cannot be reached the answer is an empty list with an error
// that says so and names nobody; it is kept only briefly, so Keycloak is not
// asked on every request while it is down, and asked again soon after.
func TestForAnswersAnEmptyListWhenKeycloakFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := newRealm()
	down := errors.New("dial tcp: connection refused")
	r.fail = map[string]error{}
	for _, s := range sites {
		r.fail[s.ClientID] = down
	}
	s := editablesites.New(sites, r, editablesites.Options{Now: func() time.Time { return now }})

	got, _, err := s.For(context.Background(), admin)
	if got == nil || len(got) != 0 {
		t.Fatalf("sites = %v, want an empty list", got)
	}
	if !errors.Is(err, editablesites.ErrUnavailable) || strings.Contains(err.Error(), admin.String()) {
		t.Fatalf("error = %v, want ErrUnavailable naming nobody", err)
	}
	calls := r.calls.Load()
	if _, cached, err := s.For(context.Background(), admin); !cached || err != nil || r.calls.Load() != calls {
		t.Fatalf("right after: cached %v, error %v, calls %d", cached, err, r.calls.Load())
	}
	r.fail = nil
	now = now.Add(11 * time.Second)
	got, cached, err := s.For(context.Background(), admin)
	if cached || err != nil || len(got) != 4 {
		t.Fatalf("after recovery: %v, cached %v, error %v", clientIDs(got), cached, err)
	}
}

// One site that cannot be read leaves only that site out; the rest of the
// answer stands. A site client the realm does not have is no failure: nobody
// edits it.
func TestForLeavesOutOnlyTheSiteThatFailed(t *testing.T) {
	t.Parallel()

	r := newRealm()
	r.fail = map[string]error{"frontend-arge": errors.New("status 500")}
	got, _, err := editablesites.New(sites, r, editablesites.Options{}).For(context.Background(), admin)
	if !errors.Is(err, editablesites.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if want := []string{"frontend-main", "frontend-artlab", "frontend-yildizjam"}; !slices.Equal(clientIDs(got), want) {
		t.Fatalf("sites = %v, want %v", clientIDs(got), want)
	}
}

// A Keycloak that does not answer cannot hold up the capabilities answer:
// after the timeout the list is empty.
func TestForGivesUpAfterTheTimeout(t *testing.T) {
	t.Parallel()

	r := newRealm()
	r.delay = time.Minute
	s := editablesites.New(sites, r, editablesites.Options{Timeout: 50 * time.Millisecond})
	start := time.Now()
	got, _, err := s.For(context.Background(), admin)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %s", elapsed)
	}
	if len(got) != 0 || !errors.Is(err, editablesites.ErrUnavailable) {
		t.Fatalf("sites = %v, error %v", clientIDs(got), err)
	}
}

// A reader that panics is a failure, not a crash of core.
func TestForSurvivesAPanickingReader(t *testing.T) {
	t.Parallel()

	r := newRealm()
	r.panics = true
	got, _, err := editablesites.New(sites, r, editablesites.Options{}).For(context.Background(), admin)
	if len(got) != 0 || !errors.Is(err, editablesites.ErrUnavailable) {
		t.Fatalf("sites = %v, error %v", clientIDs(got), err)
	}
}

// Requests of one person that arrive together share one read.
func TestForSharesOneReadBetweenConcurrentRequests(t *testing.T) {
	t.Parallel()

	r := newRealm()
	r.delay = 100 * time.Millisecond
	s := editablesites.New(sites, r, editablesites.Options{})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, _, err := s.For(context.Background(), leader); err != nil || len(got) != 3 {
				t.Errorf("sites = %v, error %v", clientIDs(got), err)
			}
		}()
	}
	wg.Wait()
	if calls := r.calls.Load(); calls != int64(len(sites)) {
		t.Fatalf("calls = %d, want %d", calls, len(sites))
	}
}

// With no site configured, or no Keycloak, the list is empty and nobody is
// asked.
func TestForWithoutSitesOrReaderIsEmpty(t *testing.T) {
	t.Parallel()

	r := newRealm()
	for _, s := range []*editablesites.Sites{
		editablesites.New(nil, r, editablesites.Options{}),
		editablesites.New(sites, nil, editablesites.Options{}),
		nil,
	} {
		got, _, err := s.For(context.Background(), admin)
		if got == nil || len(got) != 0 || err != nil {
			t.Fatalf("sites = %v, error %v", got, err)
		}
	}
	if r.calls.Load() != 0 {
		t.Fatalf("calls = %d", r.calls.Load())
	}
}

func env(value string) func(string) string {
	return func(name string) string {
		if name == editablesites.Env {
			return value
		}
		return ""
	}
}

// CMS_SITES is a JSON list of {clientId, name, url}. Unset means no site.
func TestSitesFromEnv(t *testing.T) {
	t.Parallel()

	got, err := editablesites.SitesFromEnv(env(""))
	if err != nil || len(got) != 0 {
		t.Fatalf("unset: %v, %v", got, err)
	}
	got, err = editablesites.SitesFromEnv(env(` [{"clientId":"frontend-main","name":"Ana site","url":"https://yildizskylab.com/"},` +
		`{"clientId":"frontend-yildizjam","name":"YıldızJam","url":"https://yildizjam.yildizskylab.com"}] `))
	if err != nil {
		t.Fatal(err)
	}
	want := []editablesites.Site{
		{ClientID: "frontend-main", Name: "Ana site", URL: "https://yildizskylab.com"},
		{ClientID: "frontend-yildizjam", Name: "YıldızJam", URL: "https://yildizjam.yildizskylab.com"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sites = %+v, want %+v", got, want)
	}

	for _, bad := range []string{
		`frontend-main`,
		`{"clientId":"frontend-main","name":"Ana site","url":"https://yildizskylab.com"}`,
		`[{"clientId":"","name":"Ana site","url":"https://yildizskylab.com"}]`,
		`[{"clientId":"frontend main","name":"Ana site","url":"https://yildizskylab.com"}]`,
		`[{"clientId":"frontend-main","name":" ","url":"https://yildizskylab.com"}]`,
		`[{"clientId":"frontend-main","name":"Ana site","url":"yildizskylab.com"}]`,
		`[{"clientId":"frontend-main","name":"Ana site","url":"javascript:alert(1)"}]`,
		`[{"clientId":"frontend-main","name":"Ana site","url":"https://u:p@yildizskylab.com"}]`,
		`[{"clientId":"frontend-main","name":"Ana site","url":"https://yildizskylab.com/?a=1"}]`,
		`[{"clientId":"frontend-main","name":"Ana site","url":"https://yildizskylab.com","extra":1}]`,
		`[{"clientId":"frontend-main","name":"A","url":"https://a.test"},{"clientId":"frontend-main","name":"B","url":"https://b.test"}]`,
	} {
		if _, err := editablesites.SitesFromEnv(env(bad)); !errors.Is(err, editablesites.ErrSitesInvalid) {
			t.Errorf("%s: error = %v, want ErrSitesInvalid", bad, err)
		}
	}
}
