package identity_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func privileged() authz.Principal {
	return authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
}

func member() authz.Principal {
	return authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
}

func setup(t *testing.T) (*identity.Memory, *user.MemoryStore, identity.Service) {
	t.Helper()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	svc := identity.NewService(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()))
	return dir, store, svc
}

func TestService_ListGroupsPrivilegedSeesDirectoryNotLocalTable(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	dir.PutGroup(identity.Group{ID: "g1", Name: "UYELER", Path: "/UYELER"})
	dir.PutGroup(identity.Group{ID: "g2", Name: "ADMIN", Path: "/UYELER/ADMIN"})

	groups, err := svc.ListGroups(ctx, privileged())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d groups", len(groups))
	}

	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, err := store.Get(ctx, id); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("local table should not be the roster source, got %v", err)
	}
}

func TestService_ListGroupsForbiddenForMember(t *testing.T) {
	t.Parallel()
	_, _, svc := setup(t)
	_, err := svc.ListGroups(context.Background(), member())
	if !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_PromoteAddsMembershipWithoutLocalWrite(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	dir.PutGroup(identity.Group{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"})
	ghostID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	dir.PutUser(identity.Person{
		ID:        ghostID,
		Email:     "ghost@example.com",
		FirstName: "Never",
		LastName:  "LoggedIn",
	})
	dir.Ops = nil

	if err := svc.AddMember(ctx, privileged(), "/UYELER", ghostID); err != nil {
		t.Fatal(err)
	}

	members, err := svc.Members(ctx, privileged(), "g-uyeler")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].ID != ghostID {
		t.Fatalf("roster %+v", members)
	}

	if _, err := store.Get(ctx, ghostID); !errors.Is(err, user.ErrNotFound) {
		t.Fatal("promote must not write the local shadow")
	}
	if slices.Contains(dir.Ops, "CreateUser") || slices.Contains(dir.Ops, "DeleteUser") {
		t.Fatalf("unexpected directory writes: %v", dir.Ops)
	}
	if !slices.Contains(dir.Ops, "AddMember") {
		t.Fatalf("expected AddMember, ops %v", dir.Ops)
	}
}

func TestService_RemoveMember(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	ctx := context.Background()
	dir.PutGroup(identity.Group{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"})
	id := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	dir.PutUser(identity.Person{ID: id, Email: "a@example.com"})
	if err := dir.AddMember(ctx, "g-uyeler", id); err != nil {
		t.Fatal(err)
	}

	if err := svc.RemoveMember(ctx, privileged(), "g-uyeler", id); err != nil {
		t.Fatal(err)
	}
	members, err := svc.Members(ctx, privileged(), "g-uyeler")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("got %+v", members)
	}
}

func TestService_CreateUserDualWrites(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, privileged(), identity.Person{
		Email:     "ada@example.com",
		FirstName: "Ada",
		LastName:  "Lovelace",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("expected keycloak id")
	}

	got, err := dir.GetUser(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "ada@example.com" {
		t.Fatalf("directory %+v", got)
	}

	shadow, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.Email != "ada@example.com" || shadow.FirstName != "Ada" {
		t.Fatalf("shadow %+v", shadow)
	}
}

func TestService_DeleteUserRemovesBoth(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, privileged(), identity.Person{
		Email:     "x@example.com",
		FirstName: "X",
		LastName:  "Y",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteUser(ctx, privileged(), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.GetUser(ctx, created.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("directory still has user: %v", err)
	}
	if _, err := store.Get(ctx, created.ID); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("shadow still has user: %v", err)
	}
}

func TestService_MemberCannotPromote(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	ctx := context.Background()
	dir.PutGroup(identity.Group{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"})
	id := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	dir.PutUser(identity.Person{ID: id, Email: "a@example.com"})

	err := svc.AddMember(ctx, member(), "/UYELER", id)
	if !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}
