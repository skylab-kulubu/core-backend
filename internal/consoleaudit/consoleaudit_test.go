package consoleaudit_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/consoleaudit"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

const (
	clubRealm   = "realm-uuid"
	masterRealm = "master-uuid"
	formsUUID   = "11111111-1111-1111-1111-111111111111"
)

var (
	self   = uuid.MustParse("00000000-0000-0000-0000-00000000c0de")
	admin  = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	person = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
	ykID   = "cccccccc-0000-0000-0000-000000000003"
	weblab = "dddddddd-0000-0000-0000-000000000004"
	start  = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
)

// source is Keycloak's admin event store in memory.
type source struct {
	mu      sync.Mutex
	events  []identity.AdminEvent
	reads   int
	failAll bool
	groups  map[string]identity.Group
}

func (s *source) AdminEvents(_ context.Context, from int64, max int, types []string) ([]identity.AdminEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.failAll {
		return nil, errors.New("keycloak down")
	}
	var out []identity.AdminEvent
	for _, e := range s.events {
		if e.Time >= from && slices.Contains(types, e.ResourceType) {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b identity.AdminEvent) int { return int(a.Time - b.Time) })
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func (s *source) ServiceAccountSubject(context.Context) (uuid.UUID, error) { return self, nil }

func (s *source) ClientIDOf(_ context.Context, id string) (string, error) {
	if id == formsUUID {
		return "forms", nil
	}
	return "", identity.ErrNotFound
}

func (s *source) GetGroup(_ context.Context, id string) (identity.Group, error) {
	if g, ok := s.groups[id]; ok {
		return g, nil
	}
	return identity.Group{}, identity.ErrNotFound
}

func (s *source) add(e identity.AdminEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

// notifier records what the reader told.
type notifier struct {
	mu      sync.Mutex
	members []identity.MembershipChange
	roles   []identity.GroupRolesChange
}

func (n *notifier) Notifies(identity.Group) bool { return true }
func (n *notifier) PrecheckFailed()              {}
func (n *notifier) RolesPrecheckFailed()         {}
func (n *notifier) MembershipChanged(_ context.Context, c identity.MembershipChange) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.members = append(n.members, c)
}
func (n *notifier) GroupRolesChanged(_ context.Context, c identity.GroupRolesChange) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.roles = append(n.roles, c)
}

type fixture struct {
	src   *source
	store *consoleaudit.MemoryStore
	audit *teamaudit.Memory
	note  *notifier
	svc   *consoleaudit.Service
	now   time.Time
	logs  []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		src: &source{groups: map[string]identity.Group{
			ykID:   {ID: ykID, Name: "YK", Path: "/UYELER/YK"},
			weblab: {ID: weblab, Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		}},
		store: &consoleaudit.MemoryStore{},
		audit: teamaudit.NewMemory(),
		note:  &notifier{},
		now:   start,
	}
	svc, err := consoleaudit.NewService(consoleaudit.Config{
		Source: f.src, Store: f.store, Audit: f.audit, Membership: f.note, Roles: f.note,
		Now:  func() time.Time { return f.now },
		Logf: func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	// The first pass only sets the cursor.
	if err := svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.src.reads != 0 {
		t.Fatalf("the first pass read %d pages", f.src.reads)
	}
	return f
}

func (f *fixture) pass(t *testing.T) {
	t.Helper()
	f.now = f.now.Add(time.Minute)
	if err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func event(id string, at time.Time, resourceType, op, path, rep string, author uuid.UUID, authRealm string) identity.AdminEvent {
	return identity.AdminEvent{
		ID: id, Time: at.UnixMilli(), RealmID: clubRealm, AuthRealmID: authRealm, AuthUserID: author.String(),
		ResourceType: resourceType, OperationType: op, ResourcePath: path, Representation: rep,
	}
}

func TestFirstPassStartsFromNow(t *testing.T) {
	f := newFixture(t)
	// An event before the first pass is not read.
	f.src.add(event("old", start.Add(-time.Hour), "GROUP_MEMBERSHIP", "CREATE", "users/"+person.String()+"/groups/"+ykID, "", admin, masterRealm))
	f.pass(t)
	if rows := f.audit.Rows(); len(rows) != 0 {
		t.Fatalf("rows %d", len(rows))
	}
}

func TestPrivilegedMembershipFromTheMasterConsole(t *testing.T) {
	f := newFixture(t)
	at := start.Add(30 * time.Second)
	rep := `{"id":"` + ykID + `","name":"YK","path":"/UYELER/YK"}`
	f.src.add(event("e1", at, "GROUP_MEMBERSHIP", "CREATE", "users/"+person.String()+"/groups/"+ykID, rep, admin, masterRealm))
	f.pass(t)

	rows := f.audit.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows %d", len(rows))
	}
	r := rows[0]
	if r.Action != teamaudit.ActionMemberAdd || r.Via != teamaudit.ViaConsole || r.Actor.Basis != teamaudit.BasisConsole ||
		r.Actor.ID != admin || r.Actor.Client != "keycloak:master" || r.GroupID != ykID || r.GroupPath != "/UYELER/YK" ||
		r.TargetUserID != person || r.Outcome != teamaudit.OutcomeOK || !r.OccurredAt.Equal(at.Truncate(time.Millisecond)) {
		t.Fatalf("row %+v", r)
	}
	if len(f.note.members) != 1 {
		t.Fatalf("membership notices %d", len(f.note.members))
	}
	m := f.note.members[0]
	if !m.Added || !m.Console || m.UserID != person || m.Group.Path != "/UYELER/YK" || m.ActorID != admin.String() || !m.At.Equal(r.OccurredAt) {
		t.Fatalf("notice %+v", m)
	}

	// Read again from the start (a cursor lost): no second row, no second
	// notice.
	if err := f.store.Save(context.Background(), consoleaudit.Cursor{Time: start.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	if len(f.audit.Rows()) != 1 || len(f.note.members) != 1 {
		t.Fatalf("read twice: rows %d, notices %d", len(f.audit.Rows()), len(f.note.members))
	}
	if !strings.Contains(f.svc.Prometheus(), `skylab_console_audit_events_total{outcome="duplicate"} 1`) {
		t.Fatal(f.svc.Prometheus())
	}
}

func TestCoresOwnWritesAndFailedEventsAreSkipped(t *testing.T) {
	f := newFixture(t)
	at := start.Add(time.Second)
	f.src.add(event("own", at, "GROUP_MEMBERSHIP", "CREATE", "users/"+person.String()+"/groups/"+weblab, "", self, clubRealm))
	failed := event("bad", at, "GROUP_MEMBERSHIP", "CREATE", "users/"+person.String()+"/groups/"+weblab, "", admin, clubRealm)
	failed.Error = "unknown_error"
	f.src.add(failed)
	f.pass(t)
	if len(f.audit.Rows()) != 0 || len(f.note.members) != 0 {
		t.Fatalf("rows %d, notices %d", len(f.audit.Rows()), len(f.note.members))
	}
	metrics := f.svc.Prometheus()
	for _, want := range []string{`outcome="skipped_self"} 1`, `outcome="skipped_error"} 1`} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("missing %s in\n%s", want, metrics)
		}
	}
}

func TestGroupRoleMappingFromTheClubRealm(t *testing.T) {
	f := newFixture(t)
	rep := `[{"id":"r1","name":"admin","clientRole":true,"containerId":"` + formsUUID + `"}]`
	f.src.add(event("e1", start.Add(time.Second), "CLIENT_ROLE_MAPPING", "CREATE", "groups/"+weblab+"/role-mappings/clients/"+formsUUID, rep, admin, clubRealm))
	f.src.add(event("e2", start.Add(2*time.Second), "REALM_ROLE_MAPPING", "DELETE", "groups/"+weblab+"/role-mappings/realm", `[{"name":"USER","clientRole":false}]`, admin, clubRealm))
	f.pass(t)

	rows := f.audit.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows %d", len(rows))
	}
	if r := rows[0]; r.Action != teamaudit.ActionGroupRoles || r.Actor.Client != "keycloak:realm" || r.After == nil ||
		!slices.Equal(r.After.Roles, []string{"forms:admin"}) || r.After.Operation != "CREATE" || r.Before != nil {
		t.Fatalf("added %+v %+v", r, r.After)
	}
	if r := rows[1]; r.Before == nil || !slices.Equal(r.Before.Roles, []string{":USER"}) || r.After != nil {
		t.Fatalf("removed %+v", r)
	}
	if len(f.note.roles) != 2 || !f.note.roles[0].Console || f.note.roles[0].Added[0] != (identity.ClientRole{ClientID: "forms", Role: "admin"}) ||
		f.note.roles[1].Removed[0] != (identity.ClientRole{Role: "USER"}) {
		t.Fatalf("notices %+v", f.note.roles)
	}
}

func TestDirectUserRoleIsRecordedAndWarned(t *testing.T) {
	f := newFixture(t)
	rep := `[{"name":"realm-admin","clientRole":true}]`
	f.src.add(event("e1", start.Add(time.Second), "CLIENT_ROLE_MAPPING", "CREATE", "users/"+person.String()+"/role-mappings/clients/"+formsUUID, rep, admin, masterRealm))
	f.pass(t)
	rows := f.audit.Rows()
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionUserRoles || rows[0].TargetUserID != person || rows[0].GroupPath != "" ||
		!slices.Equal(rows[0].After.Roles, []string{"forms:realm-admin"}) {
		t.Fatalf("rows %+v", rows)
	}
	if len(f.note.roles) != 0 || len(f.note.members) != 0 {
		t.Fatal("a direct role went to the Group notice")
	}
	if !slices.ContainsFunc(f.logs, func(l string) bool { return strings.Contains(l, "WARN a role was mapped to a person directly") }) {
		t.Fatalf("logs %q", f.logs)
	}
	for _, l := range f.logs {
		if strings.Contains(l, person.String()) {
			t.Fatalf("a log line names the person: %q", l)
		}
	}
}

func TestKeycloakConfigurationChanges(t *testing.T) {
	f := newFixture(t)
	f.src.add(event("c1", start.Add(time.Second), "CLIENT", "UPDATE", "clients/"+formsUUID, `{"clientId":"forms","secret":"**********"}`, admin, masterRealm))
	f.src.add(event("c2", start.Add(2*time.Second), "CLIENT_ROLE", "CREATE", "clients/"+formsUUID+"/roles/form:manage/composites",
		`[{"name":"manage-users","clientRole":true,"containerId":"unknown"},{"name":"ADMIN","clientRole":false}]`, admin, masterRealm))
	f.src.add(event("c3", start.Add(3*time.Second), "GROUP", "CREATE", "groups/"+weblab+"/children", `{"id":"eeeeeeee-0000-0000-0000-000000000005","name":"YENI","path":"/UYELER/ARGE/WEBLAB/YENI"}`, admin, masterRealm))
	f.src.add(event("c4", start.Add(4*time.Second), "GROUP", "DELETE", "groups/ffffffff-0000-0000-0000-000000000006", "", admin, masterRealm))
	f.pass(t)
	rows := f.audit.Rows()
	if len(rows) != 4 {
		t.Fatalf("rows %d", len(rows))
	}
	if r := rows[0]; r.Action != teamaudit.ActionKeycloakClient || r.After.Client != "forms" || r.After.ID != formsUUID || r.GroupPath != "" {
		t.Fatalf("client %+v", r.After)
	}
	if r := rows[1]; r.Action != teamaudit.ActionKeycloakRole || r.After.Client != "forms" || r.After.Role != "form:manage" ||
		!slices.Equal(r.After.Composites, []string{":ADMIN", "?:manage-users"}) {
		t.Fatalf("role %+v", r.After)
	}
	if r := rows[2]; r.Action != teamaudit.ActionTeamCreate || r.GroupID != "eeeeeeee-0000-0000-0000-000000000005" || r.GroupPath != "/UYELER/ARGE/WEBLAB/YENI" {
		t.Fatalf("group create %+v", r)
	}
	if r := rows[3]; r.Action != teamaudit.ActionTeamDelete || r.GroupPath != "/" {
		t.Fatalf("group delete %+v", r)
	}
	if len(f.note.members)+len(f.note.roles) != 0 {
		t.Fatal("a configuration change was told")
	}
}

func TestAWriteThatFailsIsReadAgain(t *testing.T) {
	f := newFixture(t)
	rep := `{"id":"` + weblab + `","path":"/UYELER/ARGE/WEBLAB"}`
	f.src.add(event("e1", start.Add(time.Second), "GROUP_MEMBERSHIP", "DELETE", "users/"+person.String()+"/groups/"+weblab, rep, admin, clubRealm))
	f.audit.SetUnavailable(true)
	f.now = f.now.Add(time.Minute)
	if err := f.svc.Pass(context.Background()); err == nil {
		t.Fatal("a pass whose row could not be written succeeded")
	}
	f.audit.SetUnavailable(false)
	f.pass(t)
	rows := f.audit.Rows()
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionMemberRemove || len(f.note.members) != 1 || f.note.members[0].Added {
		t.Fatalf("rows %+v, notices %+v", rows, f.note.members)
	}
}

func TestKeycloakDownKeepsTheCursor(t *testing.T) {
	f := newFixture(t)
	before, _, _ := f.store.Load(context.Background())
	f.src.failAll = true
	f.now = f.now.Add(time.Minute)
	if err := f.svc.Pass(context.Background()); err == nil {
		t.Fatal("no error")
	}
	after, _, _ := f.store.Load(context.Background())
	if after.Time != before.Time {
		t.Fatalf("cursor moved %d → %d", before.Time, after.Time)
	}
	if !strings.Contains(f.svc.Prometheus(), `skylab_console_audit_pass_total{outcome="failed"} 1`) {
		t.Fatal(f.svc.Prometheus())
	}
}

func TestAnOldCursorIsAGap(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(31 * 24 * time.Hour)
	if err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.svc.Prometheus(), "skylab_console_audit_gap_total 1") {
		t.Fatal(f.svc.Prometheus())
	}
}

func TestManyEventsAreReadPageByPage(t *testing.T) {
	f := newFixture(t)
	n := consoleaudit.PageSize + 50
	for i := range n {
		// Two events a millisecond: the cursor keeps the ids of its last one.
		at := start.Add(time.Duration(i/2) * time.Millisecond).Add(time.Second)
		f.src.add(event(fmt.Sprintf("e%03d", i), at, "CLIENT", "UPDATE", "clients/"+formsUUID, "", admin, clubRealm))
	}
	f.pass(t)
	if got := len(f.audit.Rows()); got != n {
		t.Fatalf("rows %d, want %d", got, n)
	}
	f.pass(t)
	if got := len(f.audit.Rows()); got != n {
		t.Fatalf("second pass wrote again: rows %d", got)
	}
}

func TestAnotherCoreHoldsTheLock(t *testing.T) {
	f := newFixture(t)
	f.src.add(event("e1", start.Add(time.Second), "CLIENT", "UPDATE", "clients/"+formsUUID, "", admin, clubRealm))
	err := f.store.Locked(context.Background(), func(ctx context.Context) error {
		f.pass(t)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.audit.Rows()) != 0 {
		t.Fatal("a locked pass read")
	}
	if !strings.Contains(f.svc.Prometheus(), `skylab_console_audit_pass_total{outcome="locked"} 1`) {
		t.Fatal(f.svc.Prometheus())
	}
}
