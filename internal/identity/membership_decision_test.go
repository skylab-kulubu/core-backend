package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The decision a member add makes, without the write: Privileged for every
// Group; a Leader for a Sub-team's Leader group (strictly), never for
// their own Team's Leaders or a Privileged Group; anyone else, nothing.
// Nothing is written, and no denied row.
func TestMembershipWriteDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := identity.NewMemory()
	ids := map[string]string{}
	for _, path := range []string{"/UYELER", "/UYELER/YK", "/UYELER/ARGE", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER",
		"/UYELER/ARGE/WEBLAB/AI", "/UYELER/ARGE/WEBLAB/AI/LIDERLER"} {
		ids[path] = uuid.NewString()
		m.PutGroup(identity.Group{ID: ids[path], Name: lastSegment(path), Path: path})
	}
	leader, member := uuid.New(), uuid.New()
	users := user.NewMemoryStore()
	for _, id := range []uuid.UUID{leader, member} {
		m.PutUser(identity.Person{ID: id, Email: id.String() + "@example.test"})
		if _, _, err := users.Upsert(ctx, user.User{ID: id, Email: id.String() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.AddMember(ctx, ids["/UYELER/ARGE/WEBLAB/LIDERLER"], leader); err != nil {
		t.Fatal(err)
	}
	audit := teamaudit.NewMemory()
	svc := identity.NewServiceWithOptions(m, users, authz.NewAuthorizer(onPolicy()), identity.Options{
		Delegation: identity.NewTeamDelegation(onPolicy(), m, m, identity.TeamDelegationOptions{}),
		TeamAudit:  audit,
	})

	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	d, err := svc.MembershipWriteDecision(ctx, yk, ids["/UYELER/YK"])
	if err != nil || d.Basis != teamaudit.BasisPrivileged || d.Group.Path != "/UYELER/YK" {
		t.Fatalf("privileged %+v %v", d, err)
	}

	lead := authz.Principal{ID: leader.String()}
	d, err = svc.MembershipWriteDecision(ctx, lead, ids["/UYELER/ARGE/WEBLAB/AI/LIDERLER"])
	if err != nil || d.Basis != teamaudit.BasisLeader || d.ViaGroupID != ids["/UYELER/ARGE/WEBLAB"] || d.Group.Path != "/UYELER/ARGE/WEBLAB/AI/LIDERLER" {
		t.Fatalf("leader of the Sub-team's Leaders %+v %v", d, err)
	}
	var refused *identity.DelegationError
	if _, err := svc.MembershipWriteDecision(ctx, lead, ids["/UYELER/ARGE/WEBLAB/LIDERLER"]); !errors.As(err, &refused) || refused.Code != "leader_group_needs_parent" {
		t.Fatalf("own Team's Leaders: %v", err)
	}
	if _, err := svc.MembershipWriteDecision(ctx, lead, ids["/UYELER/YK"]); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("YK by a Leader: %v", err)
	}
	if _, err := svc.MembershipWriteDecision(ctx, authz.Principal{ID: member.String()}, ids["/UYELER/ARGE/WEBLAB/AI/LIDERLER"]); err == nil {
		t.Fatal("a member decided")
	}
	if rows := audit.Rows(); len(rows) != 0 {
		t.Fatalf("a check wrote %v", rows)
	}
}

type countingNotifier struct {
	path               string
	changes, prechecks int
}

func (n *countingNotifier) Notifies(g identity.Group) bool { return g.Path == n.path }
func (n *countingNotifier) MembershipChanged(context.Context, identity.MembershipChange) {
	n.changes++
}
func (n *countingNotifier) PrecheckFailed() { n.prechecks++ }

// Joined notifiers each hear only the changes they want.
func TestJoinMembershipNotifiers(t *testing.T) {
	t.Parallel()
	if identity.JoinMembershipNotifiers(nil, nil) != nil {
		t.Fatal("nothing joined is not nil")
	}
	a, b := &countingNotifier{path: "/A"}, &countingNotifier{path: "/B"}
	if identity.JoinMembershipNotifiers(nil, a) != identity.MembershipNotifier(a) {
		t.Fatal("one is not itself")
	}
	j := identity.JoinMembershipNotifiers(a, nil, b)
	if !j.Notifies(identity.Group{Path: "/B"}) || j.Notifies(identity.Group{Path: "/C"}) {
		t.Fatal("notifies")
	}
	j.MembershipChanged(context.Background(), identity.MembershipChange{Group: identity.Group{Path: "/B"}})
	j.PrecheckFailed()
	if a.changes != 0 || b.changes != 1 || a.prechecks != 1 || b.prechecks != 1 {
		t.Fatalf("a %+v b %+v", a, b)
	}
}
