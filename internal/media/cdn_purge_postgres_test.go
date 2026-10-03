package media_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

func newPurgeQueueDatabase(t *testing.T) *media.PostgresStore {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return media.NewPostgresStore(pool)
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
