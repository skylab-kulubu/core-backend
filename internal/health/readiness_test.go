package health_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/health"
)

type fakeDatabase struct {
	mu    sync.Mutex
	err   error
	block bool
	pings int
}

func (d *fakeDatabase) Ping(ctx context.Context) error {
	d.mu.Lock()
	d.pings++
	err, block := d.err, d.block
	d.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (d *fakeDatabase) set(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

func (d *fakeDatabase) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pings
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }
func newClock() *clock                   { return &clock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)} }
func discard(string, ...any)             {}
func readiness(db health.Database, c *clock) *health.Readiness {
	return health.NewReadiness(db, health.Options{Now: c.Now, Logf: discard})
}

// The database going away and coming back moves readiness both ways, and
// draining takes the task out for good whatever the database says.
func TestReadinessFollowsTheDatabaseAndThenDraining(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{}
	c := newClock()
	r := readiness(db, c)
	ctx := context.Background()

	if err := r.Check(ctx); err != nil {
		t.Fatalf("database up: %v", err)
	}
	db.set(errors.New("connection refused"))
	c.advance(health.DefaultMaxAge)
	if err := r.Check(ctx); err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("database down: %v, want a database error", err)
	}
	db.set(nil)
	c.advance(health.DefaultMaxAge)
	if err := r.Check(ctx); err != nil {
		t.Fatalf("database back: %v", err)
	}
	r.Drain()
	if !r.Draining() {
		t.Fatal("Draining() = false after Drain")
	}
	pings := db.count()
	if err := r.Check(ctx); !errors.Is(err, health.ErrDraining) {
		t.Fatalf("draining: %v, want ErrDraining", err)
	}
	if db.count() != pings {
		t.Fatal("a draining task still pinged the database")
	}
}

// /v1/ready is public: however often it is asked, the database is pinged at
// most once per DefaultMaxAge, and the answer in between is the last one.
func TestReadinessPingsAtMostOncePerMaxAge(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{err: errors.New("down")}
	c := newClock()
	r := readiness(db, c)
	for range 50 {
		if err := r.Check(context.Background()); err == nil {
			t.Fatal("a cached failure answered ready")
		}
	}
	if got := db.count(); got != 1 {
		t.Fatalf("pings=%d for 50 checks within the max age, want 1", got)
	}
	c.advance(health.DefaultMaxAge)
	db.set(nil)
	if err := r.Check(context.Background()); err != nil {
		t.Fatalf("after the max age: %v", err)
	}
	if got := db.count(); got != 2 {
		t.Fatalf("pings=%d, want 2", got)
	}
}

// A database that does not answer is not ready within the ping timeout,
// not whenever the caller gives up.
func TestReadinessGivesUpOnADatabaseThatHangs(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{block: true}
	r := health.NewReadiness(db, health.Options{PingTimeout: 50 * time.Millisecond, Logf: discard})
	started := time.Now()
	err := r.Check(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want a deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Check took %s with a 50ms ping timeout", elapsed)
	}
}

// A caller that hangs up does not leave a failure behind for the next one.
func TestReadinessIgnoresTheCallersCancellation(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{}
	r := readiness(db, newClock())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Check(ctx); err != nil {
		t.Fatalf("cancelled caller: %v", err)
	}
}

// The log says when the database stops answering and when it is back, once
// each, not on every probe.
func TestReadinessLogsTransitionsOnly(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{}
	c := newClock()
	var mu sync.Mutex
	var lines []string
	r := health.NewReadiness(db, health.Options{Now: c.Now, Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, format)
	}})
	check := func() {
		_ = r.Check(context.Background())
		c.advance(health.DefaultMaxAge)
	}
	check()
	db.set(errors.New("down"))
	check()
	check()
	check()
	db.set(nil)
	check()
	check()
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("log lines=%q, want one when it failed and one when it came back", lines)
	}
}

// Without a database (tests, tools) readiness answers from draining alone.
func TestReadinessWithoutADatabase(t *testing.T) {
	t.Parallel()
	r := health.NewReadiness(nil, health.Options{})
	if err := r.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Drain()
	if err := r.Check(context.Background()); !errors.Is(err, health.ErrDraining) {
		t.Fatalf("err=%v, want ErrDraining", err)
	}
	var none *health.Readiness
	if err := none.Check(context.Background()); err != nil {
		t.Fatalf("nil readiness: %v", err)
	}
}
