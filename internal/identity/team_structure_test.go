package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// countingGroupCache is the Group overage cache, counting what it is told
// to forget.
type countingGroupCache struct {
	mu   sync.Mutex
	all  int
	some int
}

func (c *countingGroupCache) Forget(uuid.UUID) { c.mu.Lock(); c.some++; c.mu.Unlock() }
func (c *countingGroupCache) ForgetAll()       { c.mu.Lock(); c.all++; c.mu.Unlock() }

// structureRealm is WEBLAB with its Leader (delegationRealm) and an empty
// Sub-team FRONT, with a Privileged root that maps core's roles (the
// protection canary).
func structureRealm(t *testing.T) (m *identity.Memory, leader uuid.UUID, front string, leaders string) {
	t.Helper()
	m, leader, weblab := delegationRealm(t)
	web, err := m.GetGroup(context.Background(), weblab)
	if err != nil {
		t.Fatal(err)
	}
	front = uuid.NewString()
	m.PutGroup(identity.Group{ID: front, Name: "FRONT", Path: web.Path + "/FRONT"})
	root := uuid.NewString()
	m.PutGroup(identity.Group{ID: root, Name: "YK", Path: "/UYELER/YK"})
	if err := m.SetGroupClientRoles(context.Background(), root, []identity.ClientRole{{ClientID: "core", Role: authz.RoleGroupsManage}}); err != nil {
		t.Fatal(err)
	}
	l, err := m.GetGroup(context.Background(), "/UYELER/ARGE/WEBLAB/LIDERLER")
	if err != nil {
		t.Fatal(err)
	}
	return m, leader, front, l.ID
}

func structureService(t *testing.T, m *identity.Memory, ledger teamcode.Ledger, cache identity.GroupCache, active ...uuid.UUID) identity.Service {
	t.Helper()
	policy := onPolicy()
	// Leaders' code changes on (TEAM_DELEGATION_LEADER_CODES), so a
	// Leader's code change reaches the lock as a create and a deletion do.
	policy.LeaderTeamCodes = true
	users := user.NewMemoryStore()
	// A delegated write refuses a caller who is no active account of core.
	for _, id := range active {
		if _, _, err := users.Upsert(context.Background(), user.User{ID: id, Email: id.String() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	return identity.NewServiceWithOptions(identity.AuditedDirectory(m, teamaudit.NewMemory(), nil), users, authz.NewAuthorizer(policy), identity.Options{
		TeamCodes: ledger, GroupCache: cache,
		Delegation: identity.NewTeamDelegation(policy, m, m, identity.TeamDelegationOptions{}),
	})
}

func actorContext(id uuid.UUID) context.Context {
	return teamaudit.WithActor(context.Background(), teamaudit.Actor{ID: id, Basis: teamaudit.BasisPrivileged})
}

// A code change moves every path under the Team, so every Group overage
// read core keeps is forgotten (ForgetAll), as a rename's.
func TestChangeTeamCodeForgetsEveryKeptGroupRead(t *testing.T) {
	t.Parallel()
	m, _, front, _ := structureRealm(t)
	cache := &countingGroupCache{}
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	svc := structureService(t, m, teamcode.NewMemoryLedger(), cache)
	if _, err := svc.ChangeTeamCode(actorContext(uuid.MustParse(yk.ID)), yk, front, "FRONTEND"); err != nil {
		t.Fatal(err)
	}
	if cache.all == 0 {
		t.Error("the Group overage cache was not forgotten")
	}
}

// leaderLeavesLedger takes the Leader out of their Leader group when the
// first lock turn begins: after the decision outside the lock, before the
// write.
type leaderLeavesLedger struct {
	teamcode.Ledger
	once  sync.Once
	leave func()
}

func (l *leaderLeavesLedger) Locked(ctx context.Context, write func(context.Context, teamcode.Records) error) error {
	l.once.Do(l.leave)
	return l.Ledger.Locked(ctx, write)
}

// A Leader's write is decided again inside the lock, from Keycloak's
// membership read fresh: one who stopped leading in between changes
// nothing (TOCTOU, #227 review §4).
func TestTeamStructureDecidedAgainInsideTheLock(t *testing.T) {
	t.Parallel()
	for _, write := range []string{"create", "code", "delete"} {
		t.Run(write, func(t *testing.T) {
			t.Parallel()
			m, leader, front, leaders := structureRealm(t)
			ledger := teamcode.NewMemoryLedger()
			wrapped := &leaderLeavesLedger{Ledger: ledger, leave: func() {
				if err := m.RemoveMember(context.Background(), leaders, leader); err != nil {
					t.Error(err)
				}
			}}
			svc := structureService(t, m, wrapped, nil, leader)
			p := authz.Principal{ID: leader.String()}
			ctx := actorContext(leader)
			var err error
			switch write {
			case "create":
				_, err = svc.CreateSubTeam(ctx, p, front, identity.NewSubTeam{Code: "NEWLAB", DisplayNameTr: "Yeni"})
			case "code":
				_, err = svc.ChangeTeamCode(ctx, p, front, "FRONTEND")
			case "delete":
				err = svc.DeleteGroup(ctx, p, front)
			}
			if !errors.Is(err, identity.ErrForbidden) {
				t.Fatalf("err %v, want forbidden", err)
			}
			if g, err := m.GetGroup(context.Background(), front); err != nil || g.Name != "FRONT" {
				t.Fatalf("FRONT %+v %v", g, err)
			}
			if _, err := m.GetGroup(context.Background(), "/UYELER/ARGE/WEBLAB/FRONT/NEWLAB"); !errors.Is(err, identity.ErrNotFound) {
				t.Fatalf("NEWLAB created: %v", err)
			}
			if len(ledger.Retired()) != 0 {
				t.Fatalf("retired %v", ledger.Retired())
			}
		})
	}
}

// A Leader's deletion refused with a reason inside the lock (the target
// became protected after the decision outside it) closes its team.delete
// row as failed: Keycloak was never asked, so the row is not left pending
// (#319 review 05-2).
func TestTeamDeleteRefusedInsideTheLockClosesItsRow(t *testing.T) {
	t.Parallel()
	m, leader, front, _ := structureRealm(t)
	wrapped := &leaderLeavesLedger{Ledger: teamcode.NewMemoryLedger(), leave: func() {
		if err := m.SetGroupClientRoles(context.Background(), front, []identity.ClientRole{{ClientID: "core", Role: authz.RoleGroupsManage}}); err != nil {
			t.Error(err)
		}
	}}
	audit := teamaudit.NewMemory()
	policy := onPolicy()
	users := user.NewMemoryStore()
	if _, _, err := users.Upsert(context.Background(), user.User{ID: leader, Email: leader.String() + "@example.test"}); err != nil {
		t.Fatal(err)
	}
	svc := identity.NewServiceWithOptions(identity.AuditedDirectory(m, audit, nil), users, authz.NewAuthorizer(policy), identity.Options{
		TeamCodes: wrapped, Delegation: identity.NewTeamDelegation(policy, m, m, identity.TeamDelegationOptions{}),
	})
	err := svc.DeleteGroup(actorContext(leader), authz.Principal{ID: leader.String()}, front)
	var refused *identity.DelegationError
	if !errors.As(err, &refused) || refused.Code != authz.RefusalProtectedGroup {
		t.Fatalf("err %v, want %s", err, authz.RefusalProtectedGroup)
	}
	var deletes []teamaudit.Row
	for _, row := range audit.Rows() {
		if row.Action == teamaudit.ActionTeamDelete && row.Outcome != teamaudit.OutcomeDenied {
			deletes = append(deletes, row)
		}
	}
	if len(deletes) != 1 || deletes[0].Outcome != teamaudit.OutcomeFailed || deletes[0].ErrorCode != authz.RefusalProtectedGroup {
		t.Fatalf("team.delete rows %+v", deletes)
	}
}
