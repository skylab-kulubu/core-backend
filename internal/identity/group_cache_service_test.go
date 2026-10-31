package identity_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// notifyBus stands in for Postgres between core processes: what one says
// reaches every process at once, the speaker included (who ignores it).
type notifyBus struct {
	mu       sync.Mutex
	caches   []*identity.SharedGroupCache
	payloads []string
}

func (b *notifyBus) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	payload := args[1].(string)
	b.mu.Lock()
	b.payloads = append(b.payloads, payload)
	caches := slices.Clone(b.caches)
	b.mu.Unlock()
	for _, c := range caches {
		c.ApplyForTest(payload)
	}
	return pgconn.CommandTag{}, nil
}

// teamReplica is one core process with every cache the identity service
// keeps of Groups: Group overage, team delegation and the Owner team tree.
type teamReplica struct {
	svc        identity.Service
	groups     *identity.OverageGroups
	delegation *identity.TeamDelegation
	teamTree   *identity.TeamTree
	shared     *identity.SharedGroupCache
}

func newTeamReplica(dir *identity.Memory, notify identity.Notifier, origin string) teamReplica {
	groups := identity.NewOverageGroups(dir, identity.OverageOptions{})
	shared := identity.NewSharedGroupCache(groups, notify, noConnect, identity.SharedGroupCacheOptions{Origin: origin})
	teamTree := identity.NewTeamTree(dir, identity.TeamTreeOptions{})
	delegation := identity.NewTeamDelegation(onPolicy(), dir, dir, identity.TeamDelegationOptions{})
	az := authz.NewAuthorizer(onPolicy(), authz.WithTeamTree(teamTree), authz.WithTeamSettings(teamTree))
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), az, identity.Options{
		GroupCache: shared, TeamTree: teamTree, Delegation: delegation,
	})
	return teamReplica{svc: svc, groups: groups, delegation: delegation, teamTree: teamTree, shared: shared}
}

func twoTeamReplicas(t *testing.T) (*identity.Memory, uuid.UUID, string, *notifyBus, teamReplica, teamReplica) {
	t.Helper()
	dir, leader, weblab := delegationRealm(t)
	bus := &notifyBus{}
	writer := newTeamReplica(dir, bus, "writer")
	other := newTeamReplica(dir, bus, "other")
	bus.caches = []*identity.SharedGroupCache{writer.shared, other.shared}
	return dir, leader, weblab, bus, writer, other
}

func leaderGroupID(t *testing.T, dir *identity.Memory) string {
	t.Helper()
	g, err := dir.GetGroup(context.Background(), "/UYELER/ARGE/WEBLAB/LIDERLER")
	if err != nil {
		t.Fatal(err)
	}
	return g.ID
}

// A Team's settings written through one core process reach the other's
// Owner team tree: its next decision reads the new setting
// (horizontal-scale ticket 38).
func TestTeamSettingsOnOneReplicaReachTheOthersTeamTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, _, weblab, bus, writer, other := twoTeamReplicas(t)
	if !other.teamTree.MembersManageCompetitors("WEBLAB") {
		t.Fatal("before the write the Team's members do not manage competitors")
	}

	if _, err := writer.svc.SetTeamSettings(ctx, privileged(), weblab, identity.TeamSettings{MembersManageCompetitors: false}); err != nil {
		t.Fatal(err)
	}
	if other.teamTree.MembersManageCompetitors("WEBLAB") {
		t.Fatal("the other replica still decides by the setting from before the write")
	}
	if !slices.Equal(bus.payloads, []string{"writer *"}) {
		t.Fatalf("told %q", bus.payloads)
	}
}

// A Leader taken out of their Leader group through one core process no
// longer leads in the other at its next request; someone else's kept
// answers stay (horizontal-scale ticket 38).
func TestMembershipWriteOnOneReplicaReachesTheOthersDelegation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, leader, _, _, writer, other := twoTeamReplicas(t)
	if teams, _, err := other.delegation.ManagedTeams(ctx, leader); err != nil || len(teams) != 1 {
		t.Fatalf("before the write: %+v %v", teams, err)
	}
	if _, cached, _ := other.delegation.ManagedTeams(ctx, leader); !cached {
		t.Fatal("not kept before the write")
	}

	if err := writer.svc.RemoveMember(ctx, privileged(), leaderGroupID(t, dir), leader); err != nil {
		t.Fatal(err)
	}
	teams, cached, err := other.delegation.ManagedTeams(ctx, leader)
	if err != nil || cached || len(teams) != 0 {
		t.Fatalf("after the write the other replica answers %+v cached=%v err=%v", teams, cached, err)
	}
}

// A Group's role mappings decide what is protected: a write through one
// core process forgets the other's delegation answers for everyone.
func TestRoleMappingWriteOnOneReplicaReachesTheOthersDelegation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, leader, _, bus, writer, other := twoTeamReplicas(t)
	if _, _, err := other.delegation.ManagedTeams(ctx, leader); err != nil {
		t.Fatal(err)
	}

	if err := writer.svc.SetGroupClientRoles(ctx, privileged(), leaderGroupID(t, dir), []identity.ClientRole{{ClientID: "forms", Role: "viewer"}}); err != nil {
		t.Fatal(err)
	}
	if _, cached, err := other.delegation.ManagedTeams(ctx, leader); err != nil || cached {
		t.Fatalf("after the write: cached=%v err=%v", cached, err)
	}
	if !slices.Contains(bus.payloads, "writer *") {
		t.Fatalf("told %q", bus.payloads)
	}
}

// The joined caches are left to the writer here: ForgetAllElsewhere forgets
// nothing in this process, Forget and ForgetAll only the Group overage
// cache.
func TestSharedGroupCacheLeavesJoinedCachesToTheWriter(t *testing.T) {
	t.Parallel()
	spy, joined := &groupCacheSpy{}, &groupCacheSpy{}
	notify := &recordedNotify{}
	shared := identity.NewSharedGroupCache(spy, notify, noConnect, identity.SharedGroupCacheOptions{Origin: "a"})
	shared.Join(joined)
	person := uuid.New()

	shared.Forget(person)
	shared.ForgetAll()
	shared.ForgetAllElsewhere()
	if len(joined.forgotten) != 0 || joined.all != 0 || spy.all != 1 {
		t.Fatalf("joined forgot %v and everyone %d times; overage everyone %d times", joined.forgotten, joined.all, spy.all)
	}
	if want := []string{"a " + person.String(), "a *", "a *"}; !slices.Equal(notify.payloads, want) {
		t.Fatalf("told %q, want %q", notify.payloads, want)
	}

	shared.ApplyForTest("b " + person.String())
	shared.ApplyForTest("b *")
	if !slices.Equal(joined.forgotten, []uuid.UUID{person}) || joined.all != 1 {
		t.Fatalf("on the other processes' words joined forgot %v and everyone %d times", joined.forgotten, joined.all)
	}
}

// An app assignment through one core process forgets the other's
// delegation answers for everyone: role holders and protection follow it.
func TestAppAssignmentOnOneReplicaReachesTheOthersDelegation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, leader, weblab := delegationRealm(t)
	bus := &notifyBus{}
	audit := teamaudit.NewMemory()
	policy := onPolicy()
	policy.AppAssignments = true
	shared := identity.NewSharedGroupCache(identity.NewOverageGroups(dir, identity.OverageOptions{}), bus, noConnect, identity.SharedGroupCacheOptions{Origin: "writer"})
	writer := identity.NewServiceWithOptions(identity.AuditedDirectory(dir, audit, nil), user.NewMemoryStore(), authz.NewAuthorizer(policy), identity.Options{
		GroupCache: shared, TeamAudit: audit, AppAudit: audit, AdminGroups: dir, AppAssignments: true,
	})
	other := newTeamReplica(dir, bus, "other")
	bus.caches = []*identity.SharedGroupCache{shared, other.shared}
	if _, _, err := other.delegation.ManagedTeams(ctx, leader); err != nil {
		t.Fatal(err)
	}

	audited := teamaudit.WithActor(ctx, teamaudit.Actor{ID: uuid.New(), Basis: teamaudit.BasisPrivileged})
	if _, _, err := writer.AssignApp(audited, privileged(), "forms", identity.AppAssignmentInput{Type: "group", ID: weblab, Role: "skyforms:access"}); err != nil {
		t.Fatal(err)
	}
	if _, cached, err := other.delegation.ManagedTeams(ctx, leader); err != nil || cached {
		t.Fatalf("after the write: cached=%v err=%v", cached, err)
	}
	if !slices.Equal(bus.payloads, []string{"writer *"}) {
		t.Fatalf("told %q", bus.payloads)
	}
}
