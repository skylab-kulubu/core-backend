package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type rolesSpy struct {
	changes        []identity.GroupRolesChange
	precheckFailed int
}

func (s *rolesSpy) GroupRolesChanged(_ context.Context, change identity.GroupRolesChange) {
	s.changes = append(s.changes, change)
}

func (s *rolesSpy) RolesPrecheckFailed() { s.precheckFailed++ }

func rolesSetup(t *testing.T, dir identity.Directory) (*rolesSpy, identity.Service) {
	t.Helper()
	spy := &rolesSpy{}
	return spy, identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{GroupRoles: spy})
}

func uyeler() *identity.Memory {
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"})
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	return dir
}

// A role mapping write tells what it added and removed, once; a write that
// changes nothing tells nothing (grup-yonetimi 05).
func TestSetGroupClientRolesReportsTheChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := uyeler()
	spy, svc := rolesSetup(t, dir)
	viewer := identity.ClientRole{ClientID: "forms", Role: "viewer"}
	users := identity.ClientRole{ClientID: "core", Role: "users:manage"}

	if err := svc.SetGroupClientRoles(ctx, actor(), "g-uyeler", []identity.ClientRole{viewer, users, viewer}); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 1 {
		t.Fatalf("changes %+v", spy.changes)
	}
	got := spy.changes[0]
	if got.Group.Path != "/UYELER" || got.ActorID != actor().ID || len(got.Added) != 2 || len(got.Removed) != 0 {
		t.Fatalf("change %+v", got)
	}
	// The same mappings again: nothing changed.
	if err := svc.SetGroupClientRoles(ctx, actor(), "/UYELER", []identity.ClientRole{users, viewer}); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 1 {
		t.Fatalf("unchanged write reported: %+v", spy.changes)
	}
	if err := svc.SetGroupClientRoles(ctx, actor(), "g-uyeler", []identity.ClientRole{users}); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 2 || len(spy.changes[1].Added) != 0 || len(spy.changes[1].Removed) != 1 || spy.changes[1].Removed[0] != viewer {
		t.Fatalf("removal %+v", spy.changes)
	}
	// Nobody else may write mappings, and a refused write tells nothing.
	if err := svc.SetGroupClientRoles(ctx, member(), "g-uyeler", nil); err == nil {
		t.Fatal("a member wrote role mappings")
	}
	if len(spy.changes) != 2 {
		t.Fatalf("refused write reported: %+v", spy.changes)
	}
}

// rolesReadFails is a directory whose role mapping read fails.
type rolesReadFails struct{ *identity.Memory }

func (rolesReadFails) GroupClientRoles(context.Context, string) ([]identity.ClientRole, error) {
	return nil, errors.New("keycloak timed out")
}

func TestSetGroupClientRolesWhoseMappingsCannotBeReadWritesAndTellsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := uyeler()
	spy, svc := rolesSetup(t, rolesReadFails{mem})
	if err := svc.SetGroupClientRoles(ctx, actor(), "g-uyeler", []identity.ClientRole{{ClientID: "forms", Role: "viewer"}}); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 0 || spy.precheckFailed != 1 {
		t.Fatalf("changes %+v failures %d", spy.changes, spy.precheckFailed)
	}
	if roles, _ := mem.GroupClientRoles(ctx, "g-uyeler"); len(roles) != 1 {
		t.Fatalf("not written: %v", roles)
	}
}

// rolesWriteFailsHalfway writes the first role, then fails, as Keycloak may
// (it writes client by client).
type rolesWriteFailsHalfway struct{ *identity.Memory }

func (d rolesWriteFailsHalfway) SetGroupClientRoles(ctx context.Context, groupID string, roles []identity.ClientRole) error {
	if err := d.Memory.SetGroupClientRoles(ctx, groupID, roles[:1]); err != nil {
		return err
	}
	return errors.New("keycloak 500")
}

func TestSetGroupClientRolesThatFailsHalfwayTellsWhatChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spy, svc := rolesSetup(t, rolesWriteFailsHalfway{uyeler()})
	err := svc.SetGroupClientRoles(ctx, actor(), "g-yk", []identity.ClientRole{{ClientID: "core", Role: "audit:read"}, {ClientID: "forms", Role: "viewer"}})
	if err == nil {
		t.Fatal("the failure was hidden")
	}
	if len(spy.changes) != 1 || len(spy.changes[0].Added) != 1 || spy.changes[0].Added[0].Role != "audit:read" {
		t.Fatalf("changes %+v", spy.changes)
	}
}
