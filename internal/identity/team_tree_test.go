package identity

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

type countingLister struct {
	mu     sync.Mutex
	groups []Group
	err    error
	calls  int
}

func (l *countingLister) ListGroups(context.Context) ([]Group, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return slices.Clone(l.groups), nil
}

func (l *countingLister) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func treeGroups() []Group {
	return []Group{
		{ID: "1", Name: "UYELER", Path: "/UYELER"},
		{ID: "2", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "3", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "4", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{ID: "5", Name: "MOBIL", Path: "/UYELER/ARGE/WEBLAB/MOBIL"},
		{ID: "6", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/MOBIL/LIDERLER"},
		{ID: "7", Name: "GAMELAB", Path: "/UYELER/ARGE/GAMELAB"},
	}
}

func TestTeamTree_TeamPathAndSubTeams(t *testing.T) {
	t.Parallel()
	tree := NewTeamTree(&countingLister{groups: treeGroups()}, TeamTreeOptions{})
	if path, ok := tree.TeamPath("MOBIL"); !ok || path != "/UYELER/ARGE/WEBLAB/MOBIL" {
		t.Errorf("TeamPath(MOBIL) = %q, %v", path, ok)
	}
	if path, ok := tree.TeamPath("mobil"); !ok || path != "/UYELER/ARGE/WEBLAB/MOBIL" {
		t.Errorf("TeamPath(mobil) = %q, %v: a code compares without case", path, ok)
	}
	if _, ok := tree.TeamPath("NOPE"); ok {
		t.Error("TeamPath(NOPE) found a Group")
	}
	// LIDERLER is two Groups: no answer.
	if _, ok := tree.TeamPath("LIDERLER"); ok {
		t.Error("TeamPath(LIDERLER) answered one of two Groups")
	}
	got := tree.SubTeams("/UYELER/ARGE/WEBLAB")
	want := []string{"LIDERLER", "MOBIL", "LIDERLER"}
	if !slices.Equal(got, want) {
		t.Errorf("SubTeams = %v, want %v", got, want)
	}
	if got := tree.SubTeams("/UYELER/ARGE/WEB"); len(got) != 0 {
		t.Errorf("SubTeams of a path prefix = %v, want none", got)
	}
}

func TestTeamTree_KeepsTheTreeAMinute(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	lister := &countingLister{groups: treeGroups()}
	tree := NewTeamTree(lister, TeamTreeOptions{Now: clock})

	tree.TeamPath("MOBIL")
	tree.SubTeams("/UYELER/ARGE")
	tree.TeamPath("WEBLAB")
	if lister.count() != 1 {
		t.Fatalf("reads = %d, want 1", lister.count())
	}
	advance(DefaultTeamTreeTTL - time.Second)
	tree.TeamPath("MOBIL")
	if lister.count() != 1 {
		t.Fatalf("reads before the minute = %d, want 1", lister.count())
	}
	advance(2 * time.Second)
	tree.TeamPath("MOBIL")
	if lister.count() != 2 {
		t.Fatalf("reads after the minute = %d, want 2", lister.count())
	}
}

// A failed read answers nothing, is kept FailureTTL so Keycloak is not
// asked on every decision, and is read again after it.
func TestTeamTree_FailureAnswersNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	lister := &countingLister{err: errors.New("keycloak down")}
	var logged []string
	tree := NewTeamTree(lister, TeamTreeOptions{Now: clock, Logf: func(format string, _ ...any) { logged = append(logged, format) }})

	if _, ok := tree.TeamPath("MOBIL"); ok {
		t.Error("a failed read answered a path")
	}
	if got := tree.SubTeams("/UYELER/ARGE"); len(got) != 0 {
		t.Errorf("a failed read answered Sub-teams %v", got)
	}
	if lister.count() != 1 || len(logged) != 1 {
		t.Fatalf("reads = %d, logs = %d, want 1 and 1", lister.count(), len(logged))
	}
	lister.mu.Lock()
	lister.err, lister.groups = nil, treeGroups()
	lister.mu.Unlock()
	mu.Lock()
	now = now.Add(DefaultTeamTreeFailureTTL + time.Second)
	mu.Unlock()
	if _, ok := tree.TeamPath("MOBIL"); !ok {
		t.Error("the read after the failure is not used")
	}
}

// Decisions that arrive together share one read.
func TestTeamTree_ConcurrentReadsShareOne(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	lister := &blockingLister{countingLister: countingLister{groups: treeGroups()}, release: release}
	tree := NewTeamTree(lister, TeamTreeOptions{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tree.TeamPath("MOBIL")
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if lister.count() != 1 {
		t.Errorf("reads = %d, want 1", lister.count())
	}
}

type blockingLister struct {
	countingLister
	release chan struct{}
}

func (l *blockingLister) ListGroups(ctx context.Context) ([]Group, error) {
	<-l.release
	return l.countingLister.ListGroups(ctx)
}
