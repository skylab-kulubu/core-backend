package identity_test

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// keycloakGroups stands in for Keycloak's "a person's Groups" call. It
// answers paths per person, counts the calls and fails with err while err is
// set. While gate is set, a call reads its answer and then holds it until the
// gate closes: the answer is Keycloak's at the moment of the read, however
// late it arrives.
type keycloakGroups struct {
	mu    sync.Mutex
	paths map[uuid.UUID][]string
	err   error
	gate  chan struct{}
	calls atomic.Int64
	// deaf holds a call at the gate past its context's deadline, like a
	// call stuck before it looks at its context.
	deaf bool
	// panicWith makes a call panic with it, while set.
	panicWith any
}

func (k *keycloakGroups) GroupsForUser(ctx context.Context, userID uuid.UUID) ([]identity.Group, error) {
	k.calls.Add(1)
	k.mu.Lock()
	gate, err, deaf, panicWith := k.gate, k.err, k.deaf, k.panicWith
	paths, known := k.paths[userID]
	k.mu.Unlock()
	if panicWith != nil {
		panic(panicWith)
	}
	if gate != nil && deaf {
		<-gate
	} else if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, identity.ErrNotFound
	}
	out := make([]identity.Group, 0, len(paths))
	for _, path := range paths {
		out = append(out, identity.Group{ID: "id-" + path, Name: path[strings.LastIndex(path, "/")+1:], Path: path})
	}
	return out, nil
}

func (k *keycloakGroups) set(userID uuid.UUID, paths ...string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.paths == nil {
		k.paths = map[uuid.UUID][]string{}
	}
	k.paths[userID] = paths
}

func (k *keycloakGroups) fail(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

func (k *keycloakGroups) panicking(with any) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.panicWith = with
}

func (k *keycloakGroups) hold() chan struct{} {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gate = make(chan struct{})
	return k.gate
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newOverage(t *testing.T, kc *keycloakGroups) (*identity.OverageGroups, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	return identity.NewOverageGroups(kc, identity.OverageOptions{Now: clock.Now}), clock
}

func wantPaths(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("paths %q, want %q", got, want)
	}
}

func TestOverageGroupsAskKeycloakOnceAMinutePerPerson(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	person := uuid.New()
	kc.set(person, "/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/WEBLAB")
	groups, clock := newOverage(t, kc)
	ctx := context.Background()

	paths, cached, err := groups.Paths(ctx, person)
	if err != nil || cached {
		t.Fatalf("first answer: cached=%v err=%v", cached, err)
	}
	wantPaths(t, paths, "/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/WEBLAB")

	clock.advance(59 * time.Second)
	paths, cached, err = groups.Paths(ctx, person)
	if err != nil || !cached {
		t.Fatalf("within the minute: cached=%v err=%v", cached, err)
	}
	wantPaths(t, paths, "/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/WEBLAB")
	if got := kc.calls.Load(); got != 1 {
		t.Fatalf("Keycloak asked %d times within the minute, want 1", got)
	}

	// The minute is up: Keycloak is asked again and its new answer counts.
	kc.set(person, "/UYELER/ARGE/WEBLAB")
	clock.advance(time.Second)
	paths, cached, err = groups.Paths(ctx, person)
	if err != nil || cached {
		t.Fatalf("after the minute: cached=%v err=%v", cached, err)
	}
	wantPaths(t, paths, "/UYELER/ARGE/WEBLAB")
	if got := kc.calls.Load(); got != 2 {
		t.Fatalf("Keycloak asked %d times, want 2", got)
	}
}

func TestOverageGroupsAnswerACopy(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	person := uuid.New()
	kc.set(person, "/UYELER/ARGE/WEBLAB")
	groups, _ := newOverage(t, kc)
	paths, _, err := groups.Paths(context.Background(), person)
	if err != nil {
		t.Fatal(err)
	}
	paths[0] = "/ADMIN"
	again, _, err := groups.Paths(context.Background(), person)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths(t, again, "/UYELER/ARGE/WEBLAB")
}

func TestOverageGroupsForgetOnePersonOrEveryone(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	ada, bob := uuid.New(), uuid.New()
	kc.set(ada, "/UYELER/ARGE/WEBLAB/LIDERLER")
	kc.set(bob, "/UYELER/ARGE/GAMELAB")
	groups, _ := newOverage(t, kc)
	ctx := context.Background()
	for _, person := range []uuid.UUID{ada, bob} {
		if _, _, err := groups.Paths(ctx, person); err != nil {
			t.Fatal(err)
		}
	}

	kc.set(ada, "/UYELER/ARGE/WEBLAB")
	groups.Forget(ada)
	paths, cached, err := groups.Paths(ctx, ada)
	if err != nil || cached {
		t.Fatalf("forgotten person: cached=%v err=%v", cached, err)
	}
	wantPaths(t, paths, "/UYELER/ARGE/WEBLAB")
	if _, cached, _ := groups.Paths(ctx, bob); !cached {
		t.Fatal("forgetting one person forgot another")
	}

	groups.ForgetAll()
	for _, person := range []uuid.UUID{ada, bob} {
		if _, cached, err := groups.Paths(ctx, person); err != nil || cached {
			t.Fatalf("after ForgetAll: cached=%v err=%v", cached, err)
		}
	}
	if got := kc.calls.Load(); got != 5 {
		t.Fatalf("Keycloak asked %d times, want 5", got)
	}
}

// An unknown list is never an empty one: a failed call answers an error and
// no paths, and is not remembered.
func TestOverageGroupsNeverRememberAFailure(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	person := uuid.New()
	kc.set(person, "/UYELER/ARGE/WEBLAB")
	kc.fail(errors.New("identity: keycloak user groups failed with status 503"))
	groups, _ := newOverage(t, kc)
	ctx := context.Background()

	paths, _, err := groups.Paths(ctx, person)
	if err == nil || paths != nil {
		t.Fatalf("Keycloak down: paths %q err %v", paths, err)
	}
	if errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("a failed call reads as an unknown person: %v", err)
	}

	kc.fail(nil)
	paths, cached, err := groups.Paths(ctx, person)
	if err != nil || cached {
		t.Fatalf("after Keycloak is back: cached=%v err=%v", cached, err)
	}
	wantPaths(t, paths, "/UYELER/ARGE/WEBLAB")
	if got := kc.calls.Load(); got != 2 {
		t.Fatalf("Keycloak asked %d times, want 2", got)
	}
}

// singleflight runs a read in a goroutine of its own and panics again there
// when the read panics, which ends the process: the callers get an error.
func TestOverageGroupsAPanicInTheReadIsAnUnavailableAnswer(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	person := uuid.New()
	kc.set(person, "/UYELER/ARGE/WEBLAB")
	kc.panicking("keycloak client blew up")
	groups, _ := newOverage(t, kc)
	ctx := context.Background()

	paths, _, err := groups.Paths(ctx, person)
	if !errors.Is(err, identity.ErrGroupsUnavailable) || paths != nil {
		t.Fatalf("panicking read: paths %q err %v", paths, err)
	}
	if errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("a panic reads as an unknown person: %v", err)
	}

	kc.panicking(nil)
	paths, cached, err := groups.Paths(ctx, person)
	if err != nil || cached {
		t.Fatalf("after the panic: cached=%v err=%v", cached, err)
	}
	wantPaths(t, paths, "/UYELER/ARGE/WEBLAB")
}

func TestOverageGroupsReportAPersonKeycloakDoesNotKnow(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	groups, _ := newOverage(t, kc)
	ctx := context.Background()
	for range 2 {
		paths, _, err := groups.Paths(ctx, uuid.New())
		if !errors.Is(err, identity.ErrNotFound) || paths != nil {
			t.Fatalf("unknown person: paths %q err %v", paths, err)
		}
	}
	if got := kc.calls.Load(); got != 2 {
		t.Fatalf("an unknown person was remembered: %d calls", got)
	}
}

// Many requests of one person while their Groups are being read make one
// call to Keycloak.
func TestOverageGroupsShareOneCallPerPerson(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	person := uuid.New()
	kc.set(person, "/UYELER/ARGE/WEBLAB")
	gate := kc.hold()
	groups, _ := newOverage(t, kc)

	const callers = 20
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths, _, err := groups.Paths(context.Background(), person)
			if err == nil && !slices.Equal(paths, []string{"/UYELER/ARGE/WEBLAB"}) {
				err = errors.New("wrong paths " + strings.Join(paths, ","))
			}
			errs <- err
		}()
	}
	waitFor(t, func() bool { return kc.calls.Load() >= 1 })
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := kc.calls.Load(); got != 1 {
		t.Fatalf("Keycloak asked %d times for one person, want 1", got)
	}
}

// A membership write while a person's Groups are being read: the answer
// read before the write is not remembered, and a request after the write
// does not wait for that older read.
func TestOverageGroupsDoNotRememberAReadOlderThanAWrite(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	person := uuid.New()
	kc.set(person, "/UYELER/ARGE/WEBLAB/LIDERLER")
	gate := kc.hold()
	groups, _ := newOverage(t, kc)
	ctx := context.Background()

	before := make(chan []string, 1)
	go func() {
		paths, _, _ := groups.Paths(ctx, person)
		before <- paths
	}()
	waitFor(t, func() bool { return kc.calls.Load() == 1 })

	// The read in flight has Keycloak's answer from before the write. core
	// now removes the person from LIDERLER.
	kc.mu.Lock()
	kc.paths[person] = []string{"/UYELER/ARGE/WEBLAB"}
	kc.gate = nil
	kc.mu.Unlock()
	groups.Forget(person)

	paths, cached, err := groups.Paths(ctx, person)
	if err != nil || cached {
		t.Fatalf("after the write: cached=%v err=%v", cached, err)
	}
	wantPaths(t, paths, "/UYELER/ARGE/WEBLAB")

	close(gate)
	wantPaths(t, <-before, "/UYELER/ARGE/WEBLAB/LIDERLER")
	paths, cached, err = groups.Paths(ctx, person)
	if err != nil || !cached {
		t.Fatalf("remembered answer: cached=%v err=%v", cached, err)
	}
	wantPaths(t, paths, "/UYELER/ARGE/WEBLAB")
}

func TestOverageGroupsGiveUpOnASlowKeycloak(t *testing.T) {
	t.Parallel()
	for name, deaf := range map[string]bool{"call keeps its deadline": false, "call stuck past its deadline": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kc := &keycloakGroups{deaf: deaf}
			person := uuid.New()
			kc.set(person, "/UYELER/ARGE/WEBLAB")
			gate := kc.hold()
			defer close(gate)
			groups := identity.NewOverageGroups(kc, identity.OverageOptions{Timeout: 50 * time.Millisecond})

			started := time.Now()
			paths, _, err := groups.Paths(context.Background(), person)
			if !errors.Is(err, identity.ErrGroupsUnavailable) || paths != nil {
				t.Fatalf("slow Keycloak: paths %q err %v", paths, err)
			}
			if waited := time.Since(started); waited > 2*time.Second {
				t.Fatalf("waited %s for a slow Keycloak", waited)
			}
		})
	}
}

// A request that goes away stops waiting; the read it started still answers
// the others.
func TestOverageGroupsLetACallerLeaveWithoutFailingTheOthers(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	person := uuid.New()
	kc.set(person, "/UYELER/ARGE/WEBLAB")
	gate := kc.hold()
	groups, _ := newOverage(t, kc)

	leaving, leave := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() {
		_, _, err := groups.Paths(leaving, person)
		left <- err
	}()
	waitFor(t, func() bool { return kc.calls.Load() == 1 })
	staying := make(chan []string, 1)
	go func() {
		paths, _, _ := groups.Paths(context.Background(), person)
		staying <- paths
	}()
	// Both callers wait on the one read before the first leaves. Otherwise
	// the second could miss the cache before the read ends and ask after it
	// has, starting a read of its own.
	waitForWaitingCallers(t, 2)
	leave()
	if err := <-left; err == nil {
		t.Fatal("a caller that left got an answer")
	}
	close(gate)
	wantPaths(t, <-staying, "/UYELER/ARGE/WEBLAB")
	if got := kc.calls.Load(); got != 1 {
		t.Fatalf("Keycloak asked %d times, want 1", got)
	}
}

func TestOverageGroupsMetricsCountWithoutNamingAnyone(t *testing.T) {
	t.Parallel()
	kc := &keycloakGroups{}
	ada, bob, ghost := uuid.New(), uuid.New(), uuid.New()
	kc.set(ada, "/UYELER/ARGE/WEBLAB/LIDERLER")
	kc.set(bob, "/UYELER/ARGE/GAMELAB")
	groups, clock := newOverage(t, kc)
	ctx := context.Background()
	for _, person := range []uuid.UUID{ada, ada, bob, ghost} {
		_, _, _ = groups.Paths(ctx, person)
	}
	kc.fail(errors.New("down"))
	clock.advance(30 * time.Second)
	groups.Forget(bob)
	_, _, _ = groups.Paths(ctx, bob)

	got := groups.Prometheus()
	for _, line := range []string{
		"skylab_group_overage_requests_total 5",
		"skylab_group_overage_cache_hits_total 1",
		"skylab_group_overage_lookups_total 4",
		"skylab_group_overage_lookup_failures_total 1",
		"skylab_group_overage_unknown_subjects_total 1",
		"skylab_group_overage_people 1",
	} {
		if !strings.Contains(got, line+"\n") {
			t.Fatalf("metrics missing %q:\n%s", line, got)
		}
	}
	clock.advance(31 * time.Second)
	if got := groups.Prometheus(); !strings.Contains(got, "skylab_group_overage_people 0\n") {
		t.Fatalf("people still counted after a minute:\n%s", got)
	}
	for _, forbidden := range []string{ada.String(), bob.String(), ghost.String(), "UYELER", "{"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("metrics expose %q:\n%s", forbidden, got)
		}
	}
}

// The identity service forgets what core remembers of a person's Groups
// whenever it writes their memberships, and everyone's when a Group's name,
// and with it every path under it, changes.
type groupCacheSpy struct {
	mu        sync.Mutex
	forgotten []uuid.UUID
	all       int
}

func (s *groupCacheSpy) Forget(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgotten = append(s.forgotten, id)
}

func (s *groupCacheSpy) ForgetAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all++
}

func TestServiceForgetsCachedGroupsOnItsOwnWrites(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	spy := &groupCacheSpy{}
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		GroupCache: spy,
	})
	ctx := context.Background()
	person := uuid.New()
	dir.PutUser(identity.Person{ID: person, Email: "p@example.test"})
	dir.PutGroup(identity.Group{ID: "g-weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"})

	if err := svc.AddMember(ctx, member(), "g-weblab-l", person); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("member AddMember: %v", err)
	}
	if len(spy.forgotten) != 0 {
		t.Fatalf("a refused write forgot %v", spy.forgotten)
	}
	if err := svc.AddMember(ctx, privileged(), "g-weblab-l", person); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spy.forgotten, []uuid.UUID{person}) {
		t.Fatalf("after AddMember forgot %v, want [%s]", spy.forgotten, person)
	}
	if err := svc.RemoveMember(ctx, privileged(), "g-weblab-l", person); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spy.forgotten, []uuid.UUID{person, person}) {
		t.Fatalf("after RemoveMember forgot %v", spy.forgotten)
	}

	if _, err := svc.UpdateGroup(ctx, privileged(), "g-weblab-l", "", map[string]string{"public_listing": "true"}); err != nil {
		t.Fatal(err)
	}
	if spy.all != 0 {
		t.Fatal("an attribute change forgot everyone")
	}
	if _, err := svc.UpdateGroup(ctx, privileged(), "g-weblab-l", "KOORDINATORLER", nil); err != nil {
		t.Fatal(err)
	}
	if spy.all != 1 {
		t.Fatalf("a rename forgot everyone %d times, want 1", spy.all)
	}
}

// waitForWaitingCallers waits until n calls of Paths are blocked waiting for
// an answer, that is, have joined a read. Only the goroutine stacks show it:
// a caller between its cache check and joining the read is not blocked.
func waitForWaitingCallers(t *testing.T, n int) {
	t.Helper()
	waitFor(t, func() bool {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		waiting := 0
		for _, stack := range strings.Split(string(buf), "\n\n") {
			// "Paths(" leaves out the read itself, which runs in Paths.func1.
			if strings.Contains(stack, " [select") && strings.Contains(stack, "identity.(*OverageGroups).Paths(") {
				waiting++
			}
		}
		return waiting >= n
	})
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
