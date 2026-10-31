package identity_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// countedGroups counts the Group reads that go to Keycloak.
type countedGroups struct {
	*identity.Memory
	mu         sync.Mutex
	gets, list int
}

func (c *countedGroups) GetGroup(ctx context.Context, idOrPath string) (identity.Group, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.Memory.GetGroup(ctx, idOrPath)
}

func (c *countedGroups) ListGroups(ctx context.Context) ([]identity.Group, error) {
	c.mu.Lock()
	c.list++
	c.mu.Unlock()
	return c.Memory.ListGroups(ctx)
}

func (c *countedGroups) reset() (gets, list int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	gets, list, c.gets, c.list = c.gets, c.list, 0, 0
	return gets, list
}

// A page's current paths: a subtree query reuses the list it read and
// reads no Group again; otherwise each Group is read by its id, never a
// list of the realm, which Keycloak builds one Group's children at a time
// (review of #333, T2). A handover row names its successor (T3).
func TestTeamAuditRecordsReadGroupsOnce(t *testing.T) {
	t.Parallel()
	m := identity.NewMemory()
	ids := map[string]string{}
	for _, path := range []string{"/UYELER", "/UYELER/ARGE", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/MOBILAB"} {
		ids[path] = uuid.NewString()
		m.PutGroup(identity.Group{ID: ids[path], Name: lastSegment(path), Path: path})
	}
	gone := uuid.NewString()
	audit := teamaudit.NewMemory()
	ctx := context.Background()
	users := user.NewMemoryStore()
	holder, successor := uuid.New(), uuid.New()
	if _, _, err := users.Upsert(ctx, user.User{ID: successor, Email: "s@example.test", FirstName: "Halef", LastName: "Kişi"}); err != nil {
		t.Fatal(err)
	}
	actor := teamaudit.Actor{ID: uuid.New(), Basis: teamaudit.BasisPrivileged, Client: "admin"}
	for _, r := range []teamaudit.Record{
		{Action: teamaudit.ActionTeamMove, GroupID: ids["/UYELER/ARGE/WEBLAB"], GroupPath: "/UYELER/ARGE/WEBLAB"},
		{Action: teamaudit.ActionTeamMove, GroupID: ids["/UYELER/ARGE/WEBLAB/FRONT"], GroupPath: "/UYELER/ORG/FRONT"},
		{Action: teamaudit.ActionTeamMove, GroupID: ids["/UYELER/ARGE/MOBILAB"], GroupPath: "/UYELER/ARGE/MOBILAB"},
		{Action: teamaudit.ActionTeamDelete, GroupID: gone, GroupPath: "/UYELER/ARGE/WEBLAB/GONE"},
		{
			Action: teamaudit.ActionTermHandover, GroupID: ids["/UYELER/ARGE/MOBILAB"], GroupPath: "/UYELER/ARGE/MOBILAB", TargetUserID: holder,
			Before: &teamaudit.State{EndsAt: "2027-06-30T00:00:00Z"},
			After:  &teamaudit.State{EndsAt: "2027-06-30T00:00:00Z", SuccessorID: successor.String(), Handover: "immediate"},
		},
	} {
		r.Actor = actor
		id, err := audit.Begin(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.Finish(ctx, id, teamaudit.OutcomeOK, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	dir := &countedGroups{Memory: m}
	svc := identity.NewServiceWithOptions(dir, users, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{AuditRecords: audit})
	reader := authz.Principal{ID: uuid.NewString(), Roles: []string{authz.RoleAuditRead}}

	want := map[string]string{
		ids["/UYELER/ARGE/WEBLAB"]:       "/UYELER/ARGE/WEBLAB",
		ids["/UYELER/ARGE/WEBLAB/FRONT"]: "/UYELER/ARGE/WEBLAB/FRONT",
		ids["/UYELER/ARGE/MOBILAB"]:      "/UYELER/ARGE/MOBILAB",
	}
	check := func(name string, page identity.AuditPage, rows, gets, list int) {
		t.Helper()
		if len(page.Items) != rows {
			t.Fatalf("%s: %d rows, want %d", name, len(page.Items), rows)
		}
		for _, item := range page.Items {
			// T3: a handover names its successor; no other row has one.
			if item.Action == string(teamaudit.ActionTermHandover) {
				if item.Successor == nil || item.Successor.ID != successor || item.Successor.DisplayName != "Halef Kişi" {
					t.Fatalf("%s: handover's successor %+v", name, item.Successor)
				}
			} else if item.Successor != nil {
				t.Fatalf("%s: a %s row has a successor %+v", name, item.Action, item.Successor)
			}
			now, ok := want[item.Group.ID]
			switch {
			case !ok && item.Group.CurrentPath != nil:
				t.Fatalf("%s: gone Group %s has a current path %q", name, item.Group.ID, *item.Group.CurrentPath)
			case ok && (item.Group.CurrentPath == nil || *item.Group.CurrentPath != now):
				t.Fatalf("%s: Group %s current path %v, want %q", name, item.Group.ID, item.Group.CurrentPath, now)
			}
		}
		if g, l := dir.reset(); g != gets || l != list {
			t.Fatalf("%s: %d Group reads and %d lists, want %d and %d", name, g, l, gets, list)
		}
	}

	page, err := svc.TeamAuditRecords(ctx, reader, identity.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	check("every row", page, 5, 4, 0)

	page, err = svc.TeamAuditRecords(ctx, reader, identity.AuditQuery{GroupID: ids["/UYELER/ARGE/WEBLAB"], Subtree: true})
	if err != nil {
		t.Fatal(err)
	}
	check("WEBLAB's subtree", page, 3, 0, 1)

	page, err = svc.TeamAuditRecords(ctx, reader, identity.AuditQuery{GroupID: ids["/UYELER/ARGE/MOBILAB"]})
	if err != nil {
		t.Fatal(err)
	}
	check("MOBILAB alone", page, 2, 1, 0)
}
