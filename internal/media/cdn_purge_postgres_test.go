package media_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

func newPurgeQueueDatabase(t *testing.T) *media.PostgresStore {
	t.Helper()
	store, _ := newPurgeQueuePool(t)
	return store
}

func newPurgeQueuePool(t *testing.T) (*media.PostgresStore, *pgxpool.Pool) {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return media.NewPostgresStore(pool), pool
}

func claimedURLs(entries []media.CDNPurgeEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.URL)
	}
	slices.Sort(out)
	return out
}

func TestPostgresCDNPurgeQueueLeasesRetriesAndCompletes(t *testing.T) {
	store := newPurgeQueueDatabase(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a, b, c := "https://cdn.example.com/images/a.jpg", "https://cdn.example.com/images/b.jpg", "https://cdn.example.com/images/c.jpg"

	if err := store.EnqueueCDNPurges(ctx, []string{a, b, c}, now); err != nil {
		t.Fatal(err)
	}
	// Queuing an address again keeps one row.
	if err := store.EnqueueCDNPurges(ctx, []string{a}, now); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimCDNPurges(ctx, now, time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := claimedURLs(first); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("claimed %v", got)
	}
	// Leased: a second claim gets only the one left.
	second, err := store.ClaimCDNPurges(ctx, now, time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := claimedURLs(second); !slices.Equal(got, []string{c}) {
		t.Fatalf("second claim %v", got)
	}

	// b failed; a is queued again while its purge is in flight, so
	// completing the earlier claim leaves it.
	byURL := map[string]media.CDNPurgeEntry{}
	for _, e := range first {
		byURL[e.URL] = e
	}
	if err := store.RetryCDNPurges(ctx, []media.CDNPurgeEntry{byURL[b]}, []time.Time{now.Add(10 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Second)
	if err := store.EnqueueCDNPurges(ctx, []string{a}, later); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteCDNPurges(ctx, append([]media.CDNPurgeEntry{byURL[a]}, second...)); err != nil {
		t.Fatal(err)
	}
	count, oldest, err := store.CDNPurgeBacklog(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || oldest != time.Minute {
		t.Fatalf("backlog %d, oldest %v", count, oldest)
	}

	// After its backoff, b is due again with its failure counted.
	due, err := store.ClaimCDNPurges(ctx, now.Add(10*time.Second), time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := claimedURLs(due); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("due %v", got)
	}
	for _, e := range due {
		if want := map[string]int{a: 0, b: 1}[e.URL]; e.Attempts != want {
			t.Fatalf("%s attempts %d, want %d", e.URL, e.Attempts, want)
		}
	}

	dropped, err := store.DropCDNPurges(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("dropped %d", dropped)
	}
	if count, _, _ := store.CDNPurgeBacklog(ctx, now); count != 1 {
		t.Fatalf("left %d", count)
	}
}

// A public object deleted through the public bucket is purged at the CDN:
// R2 queues its address in the database, and the worker has Cloudflare
// purge it.
func TestPostgresCDNPurgeFromDeleteToCloudflare(t *testing.T) {
	store := newPurgeQueueDatabase(t)
	ctx := context.Background()
	fake, client := newFakeCloudflare(t)
	purger, err := media.NewCDNPurger(media.CDNPurgerConfig{Base: "https://cdn.example.com", Queue: store, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := fakeR2(t)
	r2.PurgeCDNOnChange(purger)

	if err := r2.Delete(ctx, "images/a.jpg"); err != nil {
		t.Fatal(err)
	}
	fake.set(http.StatusServiceUnavailable, false)
	if report, err := purger.Pass(ctx); err != nil || report.Failed != 1 {
		t.Fatalf("report %+v, err %v", report, err)
	}
	if count, _, _ := store.CDNPurgeBacklog(ctx, time.Now()); count != 1 {
		t.Fatalf("the failed purge was lost: %d left", count)
	}
}

// The deadlock review #178 found: a blob purge holds one connection in a
// transaction with SHARE locks while its delete queues the address. With
// requests waiting on those locks holding every other connection of the
// pool, the queue's insert on the same pool waits until the purge times
// out. On its own pool (NewCDNPurgeQueuePool) it goes through.
func TestPostgresCDNPurgeQueueDoesNotWaitOnCoresPool(t *testing.T) {
	_, base := newPurgeQueuePool(t)
	ctx := context.Background()
	shared := base.Config().Copy()
	shared.MaxConns = 2
	app, err := pgxpool.NewWithConfig(ctx, shared)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	if _, err := app.Exec(ctx, `CREATE TABLE contention_probe (id INT PRIMARY KEY, n INT NOT NULL); INSERT INTO contention_probe VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	// Connection 1: the purge's transaction, its reference check's lock held.
	purge, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer purge.Rollback(ctx)
	if _, err := purge.Exec(ctx, `SELECT id FROM contention_probe WHERE id = 1 FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	// Connection 2: a request waiting on that lock.
	waiting, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	go func() { _, _ = app.Exec(waiting, `UPDATE contention_probe SET n = n + 1 WHERE id = 1`) }()
	deadline := time.Now().Add(5 * time.Second)
	for app.Stat().AcquiredConns() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the waiting request never took the second connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	urls := []string{"https://cdn.example.com/images/a.jpg"}

	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := media.NewPostgresStore(app).EnqueueCDNPurges(short, urls, time.Now()); err == nil {
		t.Fatal("on core's exhausted pool the insert went through; the test no longer shows the wait")
	}

	own, err := media.NewCDNPurgeQueuePool(ctx, shared.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(own.Close)
	if own.Config().MaxConns != 2 {
		t.Fatalf("the queue's pool has %d connections", own.Config().MaxConns)
	}
	enough, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	if err := media.NewPostgresStore(own).EnqueueCDNPurges(enough, urls, time.Now()); err != nil {
		t.Fatalf("on its own pool: %v", err)
	}
}

func TestNewCDNPurgeQueuePoolDoesNotQuoteABadAddress(t *testing.T) {
	_, err := media.NewCDNPurgeQueuePool(context.Background(), "postgres://user:s3cret@[bad")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}
