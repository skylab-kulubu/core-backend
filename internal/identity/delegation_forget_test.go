package identity_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// heldGroups is a directory whose first read of a person's Groups reads the
// realm and then waits for release before it answers: a read that began
// before a write and ends after it.
type heldGroups struct {
	*identity.Memory
	mu      sync.Mutex
	reads   int
	started chan struct{}
	release chan struct{}
}

func newHeldGroups(m *identity.Memory) *heldGroups {
	return &heldGroups{Memory: m, started: make(chan struct{}), release: make(chan struct{})}
}

func (h *heldGroups) GroupsForUser(ctx context.Context, id uuid.UUID) ([]identity.Group, error) {
	groups, err := h.Memory.GroupsForUser(ctx, id)
	h.mu.Lock()
	h.reads++
	first := h.reads == 1
	h.mu.Unlock()
	if first {
		close(h.started)
		<-h.release
	}
	return groups, err
}

func (h *heldGroups) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads
}

// leaveLeaders takes the leader out of WEBLAB's Leader group in the realm.
func leaveLeaders(t *testing.T, m *identity.Memory, leader uuid.UUID) {
	t.Helper()
	g, err := m.GetGroup(context.Background(), "/UYELER/ARGE/WEBLAB/LIDERLER")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveMember(context.Background(), g.ID, leader); err != nil {
		t.Fatal(err)
	}
}

// A managed-Teams read that began before Forget and ends after it answers
// the requests that waited for it, but its answer is not kept, and a
// request after Forget starts a read of its own instead of joining it
// (review of #332, D1).
func TestManagedTeamsReadAcrossForgetIsNotKept(t *testing.T) {
	t.Parallel()
	for _, forget := range []string{"Forget", "ForgetAll"} {
		t.Run(forget, func(t *testing.T) {
			t.Parallel()
			m, leader, weblab := delegationRealm(t)
			dir := newHeldGroups(m)
			d := identity.NewTeamDelegation(onPolicy(), dir, m, identity.TeamDelegationOptions{})

			type result struct {
				teams []identity.ManagedTeam
				err   error
			}
			old := make(chan result, 1)
			go func() {
				teams, _, err := d.ManagedTeams(context.Background(), leader)
				old <- result{teams, err}
			}()
			<-dir.started

			leaveLeaders(t, m, leader)
			if forget == "Forget" {
				d.Forget(leader)
			} else {
				d.ForgetAll()
			}

			fresh := make(chan result, 1)
			go func() {
				teams, _, err := d.ManagedTeams(context.Background(), leader)
				fresh <- result{teams, err}
			}()
			select {
			case got := <-fresh:
				if got.err != nil || len(got.teams) != 0 {
					t.Errorf("after %s: %+v %v, want no Teams", forget, got.teams, got.err)
				}
			case <-time.After(5 * time.Second):
				close(dir.release)
				t.Fatalf("a request after %s joined the read that began before it", forget)
			}

			close(dir.release)
			if got := <-old; got.err != nil || len(got.teams) != 1 || got.teams[0].ID != weblab {
				t.Fatalf("the read before %s: %+v %v", forget, got.teams, got.err)
			}
			teams, cached, err := d.ManagedTeams(context.Background(), leader)
			if err != nil || !cached || len(teams) != 0 {
				t.Fatalf("kept after %s: %+v cached %v err %v, want the read after it", forget, teams, cached, err)
			}
		})
	}
}

// A Leader's tree read that began before ForgetAll and ends after it is not
// kept, and neither are the Leader groups it read: the next tree reads
// Keycloak again (review of #332, D1).
func TestTreeReadAcrossForgetAllIsNotKept(t *testing.T) {
	t.Parallel()
	m, leader, _ := delegationRealm(t)
	dir := newHeldGroups(m)
	d := identity.NewTeamDelegation(onPolicy(), dir, m, identity.TeamDelegationOptions{})

	done := make(chan error, 1)
	go func() {
		_, err := d.TreeFor(context.Background(), leader, false, false)
		done <- err
	}()
	<-dir.started
	leaveLeaders(t, m, leader)
	d.ForgetAll()
	close(dir.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	reads := dir.count()
	nodes, err := d.TreeFor(context.Background(), leader, false, false)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("tree after ForgetAll: %+v %v, want none", nodes, err)
	}
	if dir.count() == reads {
		t.Fatal("the tree read before ForgetAll was kept")
	}
}
