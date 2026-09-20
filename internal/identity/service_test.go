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
	svc := identity.NewServiceWithOptions(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		AccountErasureEnabled: true,
	})
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
	got := map[uuid.UUID]identity.GroupMember{}
	for _, person := range members {
		got[person.ID] = person
	}
	if len(members) != 3 {
		t.Fatalf("parent YK should list the tree, got %+v", members)
	}
	if got[nested].SourceGroupPath != "/UYELER/YK/BASKAN" || got[nested].SourceGroupID != "g-baskan" {
		t.Fatalf("nested source %+v", got[nested])
	}
	if got[deep].SourceGroupPath != "/UYELER/YK/BASKAN/YARDIMCI" {
		t.Fatalf("deep source %+v", got[deep])
	}
	if got[both].SourceGroupPath != "" || got[both].SourceGroupID != "" {
		t.Fatalf("direct parent member must not carry a subgroup source %+v", got[both])
	}
}

func TestService_RemoveMemberDropsSourceSubgroup(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	ctx := context.Background()
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	dir.PutGroup(identity.Group{ID: "g-baskan", Name: "BASKAN", Path: "/UYELER/YK/BASKAN"})
	nested := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa31")
	direct := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa32")
	dir.PutUser(identity.Person{ID: nested, Email: "nested@example.com"})
	dir.PutUser(identity.Person{ID: direct, Email: "direct@example.com"})
	if err := dir.AddMember(ctx, "g-baskan", nested); err != nil {
		t.Fatal(err)
	}
	if err := dir.AddMember(ctx, "g-yk", direct); err != nil {
		t.Fatal(err)
	}

	if err := svc.RemoveMember(ctx, privileged(), "g-yk", nested); err != nil {
		t.Fatal(err)
	}
	members, err := svc.Members(ctx, privileged(), "g-yk")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].ID != direct {
		t.Fatalf("nested member should leave the parent roster, got %+v", members)
	}
	still, err := dir.Members(ctx, "g-baskan")
	if err != nil {
		t.Fatal(err)
	}
	if len(still) != 0 {
		t.Fatalf("source subgroup should be empty, got %+v", still)
	}

	if err := svc.RemoveMember(ctx, privileged(), "g-yk", direct); err != nil {
		t.Fatal(err)
	}
	members, err = svc.Members(ctx, privileged(), "g-yk")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("direct member should leave the parent, got %+v", members)
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

func TestService_DeleteUserQueuesLifecycleWithoutPhysicalDelete(t *testing.T) {
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
	if _, err := dir.GetUser(ctx, created.ID); err != nil {
		t.Fatalf("directory identity was physically deleted before worker: %v", err)
	}
	shadow, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.AccountState != user.AccountDeletionPending {
		t.Fatalf("shadow state = %q", shadow.AccountState)
	}
	request, err := store.DeletionRequest(ctx, created.ID)
	if err != nil || request.SubjectID != created.ID || request.Status != user.DeletionRequestPending {
		t.Fatalf("request=%+v err=%v", request, err)
	}
}

func TestService_DeleteUserFailsClosedWhenAccountErasureDisabled(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	svc := identity.NewService(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()))
	ctx := context.Background()

	created, err := identity.NewServiceWithOptions(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		AccountErasureEnabled: true,
	}).CreateUser(ctx, privileged(), identity.Person{
		Email: "disabled@example.com", FirstName: "Release", LastName: "Gate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteUser(ctx, privileged(), created.ID); !errors.Is(err, identity.ErrAccountErasureDisabled) {
		t.Fatalf("delete error = %v", err)
	}
	shadow, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.AccountState != user.AccountActive {
		t.Fatalf("disabled delete changed account state to %q", shadow.AccountState)
	}
	if _, err := store.DeletionRequest(ctx, created.ID); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("disabled delete created request: %v", err)
	}
}

func TestService_DeleteUserQueuesDirectoryOnlyIdentity(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	id := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	dir.PutUser(identity.Person{ID: id, Email: "directory@example.com", FirstName: "Directory", LastName: "Only"})

	if err := svc.DeleteUser(ctx, privileged(), id); err != nil {
		t.Fatal(err)
	}
	shadow, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.AccountState != user.AccountDeletionPending {
		t.Fatalf("shadow state = %q", shadow.AccountState)
	}
	if _, err := dir.GetUser(ctx, id); err != nil {
		t.Fatalf("directory identity deleted synchronously: %v", err)
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

	named, err := svc.ListUsers(ctx, privileged(), "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(named) != 1 || named[0].ID != ghost {
		t.Fatalf("directory search %+v", named)
	}

	if _, err := svc.ListUsers(ctx, member(), "ada"); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_SearchUsersDoesNotMatchStaleDirectoryName(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	id := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	dir.PutUser(identity.Person{ID: id, Email: "member@example.com", FirstName: "LegacyName", LastName: "Lovelace"})
	if _, _, err := store.Upsert(ctx, user.User{
		ID: id, Email: "member@example.com", FirstName: "Grace", LastName: "Hopper",
	}); err != nil {
		t.Fatal(err)
	}

	stale, err := svc.ListUsers(ctx, privileged(), "LegacyName")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("stale directory name matched: %+v", stale)
	}
	current, err := svc.ListUsers(ctx, privileged(), "Grace")
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].FirstName != "Grace" || current[0].LastName != "Hopper" {
		t.Fatalf("current shadow name missing: %+v", current)
	}
}

func TestService_ListUsersSeatKeepsRoleHolders(t *testing.T) {
	t.Parallel()
	dir, _, svc := setup(t)
	ctx := context.Background()
	ghost := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	ada := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	dir.PutUser(identity.Person{ID: ghost, Email: "ghost@example.com", FirstName: "Ghost"})
	dir.PutUser(identity.Person{ID: ada, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if err := dir.AddMember(ctx, "g-yk", ada); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetGroupClientRoles(ctx, "g-yk", []identity.ClientRole{{ClientID: "dotnet", Role: "skyforms:access"}}); err != nil {
		t.Fatal(err)
	}

	seat := identity.ClientRole{ClientID: "dotnet", Role: "skyforms:access"}
	found, err := svc.ListUsers(ctx, privileged(), "ada", seat)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != ada {
		t.Fatalf("seat search %+v", found)
	}
	hidden, err := svc.ListUsers(ctx, privileged(), "ghost", seat)
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 0 {
		t.Fatalf("unseated %+v", hidden)
	}
}

func TestService_ListUsersKeepsDirectoryRowOnEmailConflict(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	owner := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	other := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	dir.PutUser(identity.Person{ID: owner, Email: "ada@example.com", FirstName: "Ada"})
	dir.PutUser(identity.Person{ID: other, Email: "ada@example.com", FirstName: "Ada"})
	if _, _, err := store.Upsert(ctx, user.User{ID: owner, Email: "ada@example.com", FirstName: "Ada"}); err != nil {
		t.Fatal(err)
	}
	all, err := svc.ListUsers(ctx, privileged(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("directory list %+v", all)
	}
}

func TestService_GetUserOmitsPhoneForUsersRead(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})
	phone := "+905559876543"
	if _, _, err := store.Upsert(ctx, user.User{
		ID: id, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", Phone: phone,
	}); err != nil {
		t.Fatal(err)
	}

	reader := authz.Principal{ID: "forms", Roles: []string{"users:read"}}
	card, err := svc.GetUser(ctx, reader, id)
	if err != nil {
		t.Fatal(err)
	}
	if card.Phone != "" {
		t.Fatalf("phone leaked %+v", card)
	}
	if card.ID != id || card.FirstName != "Ada" || card.LastName != "Lovelace" || card.Email != "ada@example.com" {
		t.Fatalf("name card %+v", card)
	}

	admin, err := svc.GetUser(ctx, privileged(), id)
	if err != nil {
		t.Fatal(err)
	}
	if admin.Phone != phone {
		t.Fatalf("privileged lost phone %+v", admin)
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
