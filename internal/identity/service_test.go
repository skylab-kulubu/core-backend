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

func TestService_MembersIncludesSubgroupTree(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	ctx := context.Background()
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	dir.PutGroup(identity.Group{ID: "g-baskan", Name: "BASKAN", Path: "/UYELER/YK/BASKAN"})
	dir.PutGroup(identity.Group{ID: "g-yardimci", Name: "YARDIMCI", Path: "/UYELER/YK/BASKAN/YARDIMCI"})
	nested := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa11")
	deep := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa12")
	both := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa13")
	dir.PutUser(identity.Person{ID: nested, Email: "nested@example.com", FirstName: "Yk", LastName: "Member"})
	dir.PutUser(identity.Person{ID: deep, Email: "deep@example.com", FirstName: "Deep", LastName: "Nested"})
	dir.PutUser(identity.Person{ID: both, Email: "both@example.com", FirstName: "Both", LastName: "Places"})
	if err := dir.AddMember(ctx, "g-baskan", nested); err != nil {
		t.Fatal(err)
	}
	if err := dir.AddMember(ctx, "g-yardimci", deep); err != nil {
		t.Fatal(err)
	}
	if err := dir.AddMember(ctx, "g-yk", both); err != nil {
		t.Fatal(err)
	}
	if err := dir.AddMember(ctx, "g-baskan", both); err != nil {
		t.Fatal(err)
	}

	members, err := svc.Members(ctx, privileged(), "g-yk")
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]struct{}{}
	for _, person := range members {
		got[person.ID] = struct{}{}
	}
	if len(members) != 3 {
		t.Fatalf("parent YK should list the tree, got %+v", members)
	}
	for _, id := range []uuid.UUID{nested, deep, both} {
		if _, ok := got[id]; !ok {
			t.Fatalf("missing %s in %+v", id, members)
		}
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
	if shadow.SkyNumber != "SKY-0000001" {
		t.Fatalf("sky number %+v", shadow)
	}
	if created.SkyNumber != "SKY-0000001" {
		t.Fatalf("created %+v", created)
	}
}

type recMail struct {
	n    int
	last user.User
}

func (r *recMail) Welcome(_ context.Context, u user.User) {
	r.n++
	r.last = u
}

func TestService_CreateUserSendsWelcome(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	rec := &recMail{}
	svc := identity.NewService(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()), rec)
	created, err := svc.CreateUser(context.Background(), privileged(), identity.Person{
		Email:     "ada@example.com",
		FirstName: "Ada",
		LastName:  "Lovelace",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.n != 1 || rec.last.ID != created.ID || rec.last.Email != "ada@example.com" {
		t.Fatalf("welcome %+v n=%d", rec.last, rec.n)
	}
	if rec.last.SkyNumber != "SKY-0000001" {
		t.Fatalf("welcome sky %+v", rec.last)
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

func TestService_UserCardInheritedVsExtraRoles(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	ctx := context.Background()
	dir.PutGroup(identity.Group{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if err := dir.AddMember(ctx, "g-weblab", id); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetGroupClientRoles(ctx, "g-weblab", []identity.ClientRole{
		{ClientID: "skyforms", Role: "skyforms:access"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.AddUserExtraRole(ctx, privileged(), id, identity.ClientRole{ClientID: "skyforms", Role: "skyforms:form:manage"}); err != nil {
		t.Fatal(err)
	}

	card, err := svc.GetUser(ctx, privileged(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(card.InheritedRoles) != 1 || card.InheritedRoles[0].Role != "skyforms:access" {
		t.Fatalf("inherited %+v", card.InheritedRoles)
	}
	if len(card.ExtraRoles) != 1 || card.ExtraRoles[0].Role != "skyforms:form:manage" {
		t.Fatalf("extra %+v", card.ExtraRoles)
	}
	if len(card.Groups) != 1 || card.Groups[0].Name != "WEBLAB" {
		t.Fatalf("groups %+v", card.Groups)
	}
}

func TestService_GetGroupAndUpdateAttributes(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	ctx := context.Background()
	dir.PutGroup(identity.Group{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})

	got, err := svc.GetGroup(ctx, privileged(), "g-weblab")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "WEBLAB" {
		t.Fatalf("got %+v", got)
	}

	updated, err := svc.UpdateGroup(ctx, privileged(), "g-weblab", "", map[string]string{"public_listing": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "WEBLAB" {
		t.Fatalf("name blanked: %+v", updated)
	}
	if updated.Attributes["public_listing"] != "true" {
		t.Fatalf("attrs %+v", updated.Attributes)
	}
}

func TestService_CreateGroupAndRename(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	ctx := context.Background()
	dir.PutGroup(identity.Group{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"})

	created, err := svc.CreateGroup(ctx, privileged(), "g-arge", "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "WEBLAB" || created.Path != "/UYELER/ARGE/WEBLAB" {
		t.Fatalf("created %+v", created)
	}

	renamed, err := svc.UpdateGroup(ctx, privileged(), created.ID, "WEBLAB-X", nil)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "WEBLAB-X" {
		t.Fatalf("renamed %+v", renamed)
	}

	if _, err := svc.CreateGroup(ctx, member(), "", "X"); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_LogoutAllSessions(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	ctx := context.Background()
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com"})
	dir.Ops = nil
	if err := svc.LogoutAllSessions(ctx, privileged(), id); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dir.Ops, "LogoutAllSessions") {
		t.Fatalf("ops %+v", dir.Ops)
	}
	if err := svc.LogoutAllSessions(ctx, member(), id); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_SearchUsersUsesShadowNotDirectory(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	ghost := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	ada := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: ghost, Email: "ghost@example.com", FirstName: "Ghost"})
	if _, _, err := store.Upsert(ctx, user.User{
		ID: ada, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", SchoolEmail: "ada@std.yildiz.edu.tr",
	}); err != nil {
		t.Fatal(err)
	}

	all, err := svc.ListUsers(ctx, privileged(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != ghost {
		t.Fatalf("directory list %+v", all)
	}

	found, err := svc.ListUsers(ctx, privileged(), "std.yildiz")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != ada || found[0].SchoolEmail != "ada@std.yildiz.edu.tr" {
		t.Fatalf("search %+v", found)
	}

	if _, err := svc.ListUsers(ctx, member(), "ada"); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_GetUserMergesSchoolEmail(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com", FirstName: "Ada"})
	if _, _, err := store.Upsert(ctx, user.User{ID: id, Email: "ada@example.com", SchoolEmail: "ada@std.yildiz.edu.tr"}); err != nil {
		t.Fatal(err)
	}
	card, err := svc.GetUser(ctx, privileged(), id)
	if err != nil {
		t.Fatal(err)
	}
	if card.SchoolEmail != "ada@std.yildiz.edu.tr" {
		t.Fatalf("card %+v", card)
	}
}

func TestService_SetGroupClientRolesForbiddenForMember(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	dir.PutGroup(identity.Group{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	err := svc.SetGroupClientRoles(context.Background(), member(), "g-weblab", []identity.ClientRole{
		{ClientID: "skyforms", Role: "skyforms:access"},
	})
	if !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_ListClientRolesPrivilegedSeesCatalog(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	dir.PutClientRole(identity.ClientRole{ClientID: "skycms", Role: "cms:access"})
	dir.PutClientRole(identity.ClientRole{ClientID: "skyforms", Role: "skyforms:access"})
	roles, err := svc.ListClientRoles(context.Background(), privileged())
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 2 || roles[0].ClientID != "skycms" || roles[1].Role != "skyforms:access" {
		t.Fatalf("roles %+v", roles)
	}
}

func TestService_ListClientRolesForbiddenForMember(t *testing.T) {
	t.Parallel()
	_, _, svc := setup(t)
	if _, err := svc.ListClientRoles(context.Background(), member()); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}
