package identity_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// membershipSpy hears of membership changes in the Groups under /UYELER/ARGE.
type membershipSpy struct {
	changes        []identity.MembershipChange
	precheckFailed int
}

func (s *membershipSpy) PrecheckFailed() { s.precheckFailed++ }

func (s *membershipSpy) Notifies(g identity.Group) bool {
	return strings.HasPrefix(g.Path, "/UYELER/ARGE/")
}

func (s *membershipSpy) MembershipChanged(_ context.Context, change identity.MembershipChange) {
	s.changes = append(s.changes, change)
}

func membershipSetup(t *testing.T) (*identity.Memory, *membershipSpy, identity.Service, uuid.UUID) {
	t.Helper()
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "g-weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
	} {
		dir.PutGroup(g)
	}
	person := uuid.New()
	dir.PutUser(identity.Person{ID: person, Email: "ada@example.test", FirstName: "Ada"})
	spy := &membershipSpy{}
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		Membership: spy,
	})
	return dir, spy, svc, person
}

func actor() authz.Principal {
	return authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Groups: []string{"/UYELER/YK"}}
}

func TestAddMemberNotifiesOneChangePerActualAdd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, spy, svc, person := membershipSetup(t)

	if err := svc.AddMember(ctx, actor(), "g-weblab", person); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 1 {
		t.Fatalf("changes %v", spy.changes)
	}
	got := spy.changes[0]
	if !got.Added || got.UserID != person || got.ActorID != actor().ID || got.Group.Path != "/UYELER/ARGE/WEBLAB" {
		t.Fatalf("change %+v", got)
	}
	// Already a member: Keycloak takes the add again, but nothing changed.
	if err := svc.AddMember(ctx, actor(), "/UYELER/ARGE/WEBLAB", person); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 1 {
		t.Fatalf("re-add notified: %v", spy.changes)
	}
	// A member of the team joining its leaders is a change of its own.
	if err := svc.AddMember(ctx, actor(), "g-weblab-l", person); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 2 || spy.changes[1].Group.Path != "/UYELER/ARGE/WEBLAB/LIDERLER" {
		t.Fatalf("leaders add: %v", spy.changes)
	}
}

func TestAddMemberNotifiesNothingOnFailureOrForOtherGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, spy, svc, person := membershipSetup(t)

	if err := svc.AddMember(ctx, actor(), "g-weblab", uuid.New()); err == nil {
		t.Fatal("unknown person added")
	}
	if err := svc.AddMember(ctx, member(), "g-weblab", person); err == nil {
		t.Fatal("member allowed to add")
	}
	dir.Ops = nil
	if err := svc.AddMember(ctx, actor(), "g-yk", person); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 0 {
		t.Fatalf("changes %v", spy.changes)
	}
	// A Group the notifier does not want costs no membership read.
	if slices.Contains(dir.Ops, "GroupsForUser") {
		t.Fatalf("ops %v", dir.Ops)
	}
	// Named by path, it costs no read at all.
	dir.Ops = nil
	if err := svc.AddMember(ctx, actor(), "/UYELER/YK", person); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(dir.Ops, "GetGroup") || slices.Contains(dir.Ops, "GroupsForUser") {
		t.Fatalf("ops %v", dir.Ops)
	}
}

// slowGroups is a directory whose membership read fails, after checking
// that it was given a deadline of its own.
type slowGroups struct {
	*identity.Memory
	deadline bool
}

func (d *slowGroups) GroupsForUser(ctx context.Context, _ uuid.UUID) ([]identity.Group, error) {
	_, d.deadline = ctx.Deadline()
	return nil, errors.New("keycloak timed out")
}

func TestAddMemberWhoseMembershipCannotBeReadAddsAndSendsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem, _, _, person := membershipSetup(t)
	dir := &slowGroups{Memory: mem}
	spy := &membershipSpy{}
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{Membership: spy})

	if err := svc.AddMember(ctx, actor(), "g-weblab", person); err != nil {
		t.Fatalf("the add failed with the read: %v", err)
	}
	if len(spy.changes) != 0 || spy.precheckFailed != 1 || !dir.deadline {
		t.Fatalf("changes %v precheck failures %d deadline %v", spy.changes, spy.precheckFailed, dir.deadline)
	}
	if groups, _ := mem.GroupsForUser(ctx, person); len(groups) != 1 {
		t.Fatalf("groups %v", groups)
	}
}

func TestRemoveMemberNotifiesTheSourceGroupOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, spy, svc, person := membershipSetup(t)
	if err := dir.AddMember(ctx, "g-weblab-l", person); err != nil {
		t.Fatal(err)
	}

	// Removed from the team's roster, the person leaves the Group they sit in.
	if err := svc.RemoveMember(ctx, actor(), "g-weblab", person); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 1 {
		t.Fatalf("changes %v", spy.changes)
	}
	got := spy.changes[0]
	if got.Added || got.UserID != person || got.Group.Path != "/UYELER/ARGE/WEBLAB/LIDERLER" || got.ActorID != actor().ID {
		t.Fatalf("change %+v", got)
	}
	// No longer a member: nothing to tell.
	if err := svc.RemoveMember(ctx, actor(), "g-weblab", person); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 1 {
		t.Fatalf("second removal notified: %v", spy.changes)
	}
}

func TestRemoveMemberFromAnUnwantedGroupNotifiesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, spy, svc, person := membershipSetup(t)
	if err := dir.AddMember(ctx, "g-yk", person); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveMember(ctx, actor(), "g-yk", person); err != nil {
		t.Fatal(err)
	}
	if len(spy.changes) != 0 {
		t.Fatalf("changes %v", spy.changes)
	}
}
