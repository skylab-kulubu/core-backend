package dashboard_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

func putMember(t *testing.T, dir *identity.Memory, group string, p identity.Person) {
	t.Helper()
	dir.PutUser(p)
	if err := dir.AddMember(t.Context(), group, p.ID); err != nil {
		t.Fatal(err)
	}
}

// seedMembers puts five people in the UYELER tree and one outside it.
// WEBLAB is publicly listed, GECEKODU is not.
func seedMembers(t *testing.T, f *fixture) (ada, bob, cem, dil, eda uuid.UUID) {
	t.Helper()
	public := map[string]string{"public_listing": "true"}
	for _, g := range []identity.Group{
		{ID: "u", Name: "UYELER", Path: "/UYELER"},
		{ID: "arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: public},
		{ID: "weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{ID: "gk", Name: "GECEKODU", Path: "/UYELER/GECEKODU"},
		{ID: "other", Name: "MISAFIR", Path: "/MISAFIR"},
	} {
		f.dir.PutGroup(g)
	}
	ada, bob, cem, dil, eda = uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	putMember(t, f.dir, "weblab-l", identity.Person{ID: ada, FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", CreatedAt: at(now.Add(-time.Hour))})
	if err := f.dir.AddMember(t.Context(), "gk", ada); err != nil {
		t.Fatal(err)
	}
	putMember(t, f.dir, "weblab", identity.Person{ID: bob, FirstName: "Bob", LastName: "B", Email: "bob@example.com", CreatedAt: at(now.AddDate(0, -1, 0))})
	putMember(t, f.dir, "u", identity.Person{ID: cem, FirstName: "Cem", LastName: "C", CreatedAt: at(now.AddDate(-2, 0, 0))})
	// Disabled in Keycloak.
	putMember(t, f.dir, "gk", identity.Person{ID: dil, FirstName: "Dil", LastName: "D", CreatedAt: at(now.Add(-2 * time.Hour))})
	if err := f.dir.DisableUser(t.Context(), dil); err != nil {
		t.Fatal(err)
	}
	// Being erased: core holds the deletion marker.
	putMember(t, f.dir, "gk", identity.Person{ID: eda, FirstName: "Eda", LastName: "E", CreatedAt: at(now.Add(-3 * time.Hour))})
	f.store.block(eda)
	// Not a Member: outside the UYELER tree.
	putMember(t, f.dir, "other", identity.Person{ID: uuid.New(), FirstName: "Fikret", CreatedAt: at(now)})
	return
}

func joinerIDs(m *dashboard.Members) []uuid.UUID {
	out := make([]uuid.UUID, len(m.RecentJoiners))
	for i, j := range m.RecentJoiners {
		out[i] = j.ID
	}
	return out
}

// The Members section is the User read decision of the user list: only a
// person who may read people gets it. It leaves out people disabled in
// Keycloak and people core is erasing, answers no contact, and names people
// as core's other people reads do: with the names core stores.
func TestSummaryMembersAreActiveMembersWithoutContact(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ada, bob, cem, _, _ := seedMembers(t, f)
	f.store.names[bob] = dashboard.Account{FirstName: "Robert", LastName: "Bee"}
	got, err := f.svc.Summary(t.Context(), privileged)
	if err != nil {
		t.Fatal(err)
	}
	m := got.Members
	if m == nil || got.MembersUnavailable {
		t.Fatalf("members %+v unavailable %v", m, got.MembersUnavailable)
	}
	if m.Active != 3 {
		t.Fatalf("active %d", m.Active)
	}
	if len(m.NewByMonth) != dashboard.MemberMonths || m.NewByMonth[0].Month != "2025-11" || m.NewByMonth[11].Month != "2026-10" {
		t.Fatalf("months %v", m.NewByMonth)
	}
	if m.NewByMonth[11].Count != 1 || m.NewByMonth[10].Count != 1 {
		t.Fatalf("month counts %v", m.NewByMonth)
	}
	if !slices.Equal(joinerIDs(m), []uuid.UUID{ada, bob, cem}) {
		t.Fatalf("joiners %+v", m.RecentJoiners)
	}
	if j := m.RecentJoiners[1]; j.FirstName != "Robert" || j.LastName != "Bee" {
		t.Fatalf("core's stored name is not the one answered: %+v", j)
	}
	if j := m.RecentJoiners[0]; j.FirstName != "Ada" {
		t.Fatalf("a person core has no row for keeps Keycloak's name: %+v", j)
	}
	// Privileged: every team, the unlisted GECEKODU too.
	if !slices.Equal(m.RecentJoiners[0].Teams, []string{"GECEKODU", "WEBLAB"}) || !slices.Equal(m.RecentJoiners[1].Teams, []string{"WEBLAB"}) || len(m.RecentJoiners[2].Teams) != 0 {
		t.Fatalf("teams %+v", m.RecentJoiners)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "@") || strings.Contains(string(raw), "email") || strings.Contains(string(raw), "phone") {
		t.Fatalf("the summary carries contact data: %s", raw)
	}
}

// users:read lets a person read people but not manage them: they see the
// joiners' publicly listed teams only. A product's service account is not a
// person and gets no Members section, whatever its roles.
func TestSummaryMembersTeamsAndServiceAccounts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ada, _, _, _, _ := seedMembers(t, f)
	reader := authz.Principal{ID: uuid.NewString(), Roles: []string{"users:read"}}
	got, err := f.svc.Summary(t.Context(), reader)
	if err != nil || got.Members == nil {
		t.Fatalf("users:read reads people: %+v %v", got.Members, err)
	}
	if j := got.Members.RecentJoiners[0]; j.ID != ada || !slices.Equal(j.Teams, []string{"WEBLAB"}) {
		t.Fatalf("a reader sees only listed teams: %+v", j)
	}
	for _, p := range []authz.Principal{
		{ID: uuid.NewString(), Roles: []string{"users:read"}, Product: authz.ProductForms},
		{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}, Product: authz.ProductForms},
	} {
		got, err := f.svc.Summary(t.Context(), p)
		if err != nil || got.Members != nil || got.MembersUnavailable {
			t.Fatalf("service account %+v: members %+v unavailable %v err %v", p, got.Members, got.MembersUnavailable, err)
		}
	}
}

// gatedDirectory is the memory directory with Keycloak's failure modes: it
// counts tree reads, can hold them until released, and can fail them.
type gatedDirectory struct {
	*identity.Memory
	reads atomic.Int64
	mu    sync.Mutex
	gate  chan struct{}
	fail  bool
}

func (d *gatedDirectory) GetGroup(ctx context.Context, ref string) (identity.Group, error) {
	d.reads.Add(1)
	d.mu.Lock()
	gate, fail := d.gate, d.fail
	d.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return identity.Group{}, ctx.Err()
		}
	}
	if fail {
		return identity.Group{}, errors.New("identity: keycloak group by path failed with status 503")
	}
	return d.Memory.GetGroup(ctx, ref)
}

func (d *gatedDirectory) hold() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gate = make(chan struct{})
	return d.gate
}

func (d *gatedDirectory) setFail(fail bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fail = fail
	d.gate = nil
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

type cacheEnv struct {
	f    *fixture
	dir  *gatedDirectory
	logs *logLines
	svc  dashboard.Service
	ada  uuid.UUID
}

func newCacheEnv(t *testing.T) *cacheEnv {
	t.Helper()
	f := newFixture(t)
	ada, _, _, _, _ := seedMembers(t, f)
	e := &cacheEnv{f: f, dir: &gatedDirectory{Memory: f.dir}, logs: &logLines{}, ada: ada}
	svc, err := dashboard.NewService(dashboard.Options{
		Events: f.events, Store: f.store, Authz: authz.NewAuthorizer(authz.DefaultPolicy()),
		Directory: e.dir, MembersTTL: time.Minute, MembersReadTimeout: 200 * time.Millisecond,
		MembersFailureTTL: 30 * time.Second, Now: f.clock.now, Logf: e.logs.logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

func (e *cacheEnv) members(t *testing.T, ctx context.Context) dashboard.Summary {
	t.Helper()
	got, err := e.svc.Summary(ctx, privileged)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Callers arriving during a read share it; one that goes away stops waiting
// without failing the others.
func TestMembersReadIsSharedAndACallerMayLeave(t *testing.T) {
	t.Parallel()
	e := newCacheEnv(t)
	gate := e.dir.hold()
	leaving, leave := context.WithCancel(context.Background())
	left := make(chan dashboard.Summary, 1)
	go func() { left <- e.members(t, leaving) }()
	eventually(t, func() bool { return e.dir.reads.Load() == 1 })
	staying := make(chan dashboard.Summary, 1)
	go func() { staying <- e.members(t, context.Background()) }()
	leave()
	if got := <-left; got.Members != nil || !got.MembersUnavailable {
		t.Fatalf("a caller that left: %+v", got.Members)
	}
	close(gate)
	if got := <-staying; got.Members == nil || got.Members.Active != 3 {
		t.Fatalf("the caller that stayed: %+v %v", got.Members, got.MembersUnavailable)
	}
	if n := e.dir.reads.Load(); n != 1 {
		t.Fatalf("Keycloak read %d times, want 1", n)
	}
}

// One read of the tree serves everyone for its TTL; an erasure started
// meanwhile shows at once. Past the TTL the old read is answered at once and
// a new one runs in the background.
func TestMembersTreeIsReadOncePerTTLAndRefreshedInTheBackground(t *testing.T) {
	t.Parallel()
	e := newCacheEnv(t)
	e.members(t, t.Context())
	e.f.store.block(e.ada)
	e.f.clock.set(now.Add(59 * time.Second))
	got := e.members(t, t.Context())
	if e.dir.reads.Load() != 1 {
		t.Fatal("the tree was read again within its TTL")
	}
	if got.Members.Active != 2 || slices.Contains(joinerIDs(got.Members), e.ada) {
		t.Fatalf("an erasure waits for the cache: %+v", got.Members)
	}

	gate := e.dir.hold()
	e.f.clock.set(now.Add(time.Minute))
	got = e.members(t, t.Context())
	if got.Members == nil || !got.Members.AsOf.Equal(now) {
		t.Fatalf("past its TTL the old read is answered while the new one runs: %+v", got.Members)
	}
	close(gate)
	eventually(t, func() bool {
		got := e.members(t, t.Context())
		return got.Members != nil && got.Members.AsOf.Equal(now.Add(time.Minute))
	})
	if n := e.dir.reads.Load(); n != 2 {
		t.Fatalf("Keycloak read %d times, want 2", n)
	}
}

// A read that hangs gives up after its timeout. With nothing read before,
// the section is unavailable, and the failure is kept for a while so that
// Keycloak is not asked on every request; one PII-free line is logged.
func TestMembersReadTimesOutAndItsFailureIsKept(t *testing.T) {
	t.Parallel()
	e := newCacheEnv(t)
	e.dir.hold() // never released
	got := e.members(t, t.Context())
	if got.Members != nil || !got.MembersUnavailable {
		t.Fatalf("hung Keycloak: %+v", got.Members)
	}
	e.dir.setFail(true)
	e.f.clock.set(now.Add(29 * time.Second))
	if got := e.members(t, t.Context()); !got.MembersUnavailable || e.dir.reads.Load() != 1 {
		t.Fatalf("a kept failure: unavailable %v, reads %d", got.MembersUnavailable, e.dir.reads.Load())
	}
	e.dir.setFail(false)
	e.f.clock.set(now.Add(30 * time.Second))
	if got := e.members(t, t.Context()); got.Members == nil || e.dir.reads.Load() != 2 {
		t.Fatalf("after the failure expired: %+v, reads %d", got.Members, e.dir.reads.Load())
	}
	lines := e.logs.all()
	if len(lines) != 1 || !strings.Contains(lines[0], `"event":"dashboard_members"`) || !strings.Contains(lines[0], "deadline") {
		t.Fatalf("log lines %q", lines)
	}
	for _, name := range []string{"Ada", "Lovelace", e.ada.String(), "@"} {
		if strings.Contains(lines[0], name) {
			t.Fatalf("the log line names someone: %q", lines[0])
		}
	}
}

// When a refresh fails, the last good read is answered on.
func TestMembersFailedRefreshKeepsTheLastRead(t *testing.T) {
	t.Parallel()
	e := newCacheEnv(t)
	e.members(t, t.Context())
	e.dir.setFail(true)
	e.f.clock.set(now.Add(2 * time.Minute))
	e.members(t, t.Context())
	eventually(t, func() bool { return len(e.logs.all()) == 1 })
	got := e.members(t, t.Context())
	if got.Members == nil || got.MembersUnavailable || !got.Members.AsOf.Equal(now) || got.Members.Active != 3 {
		t.Fatalf("after a failed refresh: %+v %v", got.Members, got.MembersUnavailable)
	}
	if n := e.dir.reads.Load(); n != 2 {
		t.Fatalf("Keycloak read %d times, want 2 (the failure is kept)", n)
	}
}

// Keycloak down costs the Members section, not the summary.
func TestSummaryWithoutTheDirectoryKeepsTheRest(t *testing.T) {
	t.Parallel()
	e := newCacheEnv(t)
	e.dir.setFail(true)
	got := e.members(t, t.Context())
	if got.Members != nil || !got.MembersUnavailable || got.Events.Total != 5 {
		t.Fatalf("members %+v unavailable %v events %+v", got.Members, got.MembersUnavailable, got.Events)
	}
	// A caller who may not read people is not told the directory is down.
	got, err := e.svc.Summary(t.Context(), leader)
	if err != nil || got.MembersUnavailable {
		t.Fatalf("unavailable %v err %v", got.MembersUnavailable, err)
	}
}
