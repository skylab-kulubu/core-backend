package media_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

const testZone = "0123456789abcdef0123456789abcdef"

// fakeCloudflare answers Cloudflare's purge-by-URL call
// (POST /zones/{zone}/purge_cache) and records each call's files.
type fakeCloudflare struct {
	mu    sync.Mutex
	calls [][]string
	auth  []string
	// status is what the next calls answer with; zero is 200.
	status int
	// failBody answers 200 with success false.
	failBody bool
	// rejects answers 400, as Cloudflare does for an address it refuses,
	// to every call naming an address that contains it.
	rejects string
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Files []string `json:"files"`
	}
	if r.Method != http.MethodPost || r.URL.Path != "/zones/"+testZone+"/purge_cache" {
		http.Error(w, "unexpected call", http.StatusNotFound)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, body.Files)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	status, failBody, rejects := f.status, f.failBody, f.rejects
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if rejects != "" && slices.ContainsFunc(body.Files, func(file string) bool { return strings.Contains(file, rejects) }) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1016,"message":"Invalid url"}]}`))
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error for https://cdn.example.com/x"}]}`))
		return
	}
	if failBody {
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1012,"message":"Request must contain one of purge_everything, files, tags, hosts or prefixes"}]}`))
		return
	}
	_, _ = w.Write([]byte(`{"success":true,"errors":[],"messages":[],"result":{"id":"` + testZone + `"}}`))
}

func (f *fakeCloudflare) set(status int, failBody bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.failBody = status, failBody
}

func (f *fakeCloudflare) purged() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func newFakeCloudflare(t *testing.T) (*fakeCloudflare, media.CloudflarePurge) {
	t.Helper()
	fake := &fakeCloudflare{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return fake, media.CloudflarePurge{API: srv.URL, ZoneID: testZone, Token: "purge-token"}
}

// clock is a pass's time, moved by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newPurger(t *testing.T, client media.CDNPurgeClient) (*media.CDNPurger, *media.MemoryCDNPurgeQueue, *clock) {
	t.Helper()
	queue := media.NewMemoryCDNPurgeQueue()
	at := &clock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	purger, err := media.NewCDNPurger(media.CDNPurgerConfig{
		Base: "https://cdn.example.com/", Queue: queue, Client: client, Now: at.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return purger, queue, at
}

func TestCDNPurgeConfigFromEnv(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		env     map[string]string
		enabled bool
		wantErr bool
	}{
		{name: "both unset is off", env: map[string]string{}},
		{name: "both set is on", env: map[string]string{media.CDNPurgeZoneEnv: testZone, media.CDNPurgeTokenEnv: "token"}, enabled: true},
		{name: "zone alone", env: map[string]string{media.CDNPurgeZoneEnv: testZone}, wantErr: true},
		{name: "token alone", env: map[string]string{media.CDNPurgeTokenEnv: "token"}, wantErr: true},
		{name: "zone not an id", env: map[string]string{media.CDNPurgeZoneEnv: "yildizskylab.com", media.CDNPurgeTokenEnv: "token"}, wantErr: true},
		{name: "token with a space", env: map[string]string{media.CDNPurgeZoneEnv: testZone, media.CDNPurgeTokenEnv: "Bearer token"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, err := media.CDNPurgeConfigFromEnv(func(name string) string { return test.env[name] })
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err == nil && config.Enabled() != test.enabled {
				t.Fatalf("enabled = %v", config.Enabled())
			}
			if err != nil && strings.Contains(err.Error(), "token\"") {
				t.Fatalf("the error names the token: %v", err)
			}
		})
	}
}

func TestCloudflarePurgeSendsTheURLsWithTheToken(t *testing.T) {
	t.Parallel()
	fake, client := newFakeCloudflare(t)

	urls := []string{"https://cdn.example.com/images/a.jpg", "https://cdn.example.com/images/a.jpg/card.jpg"}
	if err := client.PurgeURLs(context.Background(), urls); err != nil {
		t.Fatal(err)
	}
	if got := fake.purged(); len(got) != 1 || !slices.Equal(got[0], urls) {
		t.Fatalf("calls %v", got)
	}
	if fake.auth[0] != "Bearer purge-token" {
		t.Fatalf("Authorization %q", fake.auth[0])
	}
}

func TestCloudflarePurgeFailsOnAnErrorStatusOrAnUnsuccessfulAnswer(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		status   int
		failBody bool
		code     string
	}{
		{status: http.StatusForbidden, code: "10000"},
		{status: http.StatusTooManyRequests, code: "429"},
		{failBody: true, code: "1012"},
	} {
		fake, client := newFakeCloudflare(t)
		fake.set(test.status, test.failBody)
		err := client.PurgeURLs(context.Background(), []string{"https://cdn.example.com/images/a.jpg"})
		if err == nil || !strings.Contains(err.Error(), test.code) {
			t.Fatalf("status %d: err = %v", test.status, err)
		}
		// Cloudflare's messages may echo an address; the error names
		// neither an address nor the token.
		if strings.Contains(err.Error(), "cdn.example.com") || strings.Contains(err.Error(), "purge-token") {
			t.Fatalf("the error names an address or the token: %v", err)
		}
	}
}

func TestCDNPurgerQueuesTheAddressOfAKey(t *testing.T) {
	t.Parallel()
	purger, queue, _ := newPurger(t, media.CloudflarePurge{})
	ctx := context.Background()

	for _, key := range []string{"images/a.jpg", "/images/a.jpg/card.jpg", "files/0b0c", "https://elsewhere.example.org/legacy.png", ""} {
		if err := purger.QueueKey(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"https://cdn.example.com/files/0b0c", "https://cdn.example.com/images/a.jpg", "https://cdn.example.com/images/a.jpg/card.jpg"}
	if got := queue.URLs(); !slices.Equal(got, want) {
		t.Fatalf("queued %v, want %v", got, want)
	}
}

// A key is a path: each of its segments is escaped as a browser asks for
// it, so the purge names the address the CDN cached.
func TestCDNPurgerEscapesEachSegmentOfAKey(t *testing.T) {
	t.Parallel()
	purger, queue, _ := newPurger(t, media.CloudflarePurge{})
	if err := purger.QueueKey(context.Background(), "files/a b/ç?#.pdf"); err != nil {
		t.Fatal(err)
	}
	if got, want := queue.URLs(), []string{"https://cdn.example.com/files/a%20b/%C3%A7%3F%23.pdf"}; !slices.Equal(got, want) {
		t.Fatalf("queued %v, want %v", got, want)
	}
}

// One address Cloudflare refuses (400) must not hold up the others in its
// batch: the batch is tried address by address, and the refused one is
// dropped and counted.
func TestCDNPurgerDropsAnAddressCloudflareRefusesAndPurgesTheRest(t *testing.T) {
	t.Parallel()
	fake, client := newFakeCloudflare(t)
	purger, queue, _ := newPurger(t, client)
	ctx := context.Background()
	for _, key := range []string{"images/a.jpg", "images/bad.jpg", "images/c.jpg"} {
		if err := purger.QueueKey(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	fake.rejects = "bad"
	fake.mu.Unlock()

	report, err := purger.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Purged != 2 || report.Rejected != 1 || report.Failed != 0 || report.Err != nil || len(queue.URLs()) != 0 {
		t.Fatalf("report %+v, left %v", report, queue.URLs())
	}
	if calls := fake.purged(); len(calls) != 4 {
		t.Fatalf("calls %v: the batch, then each address", calls)
	}
	if !strings.Contains(purger.Prometheus(), `skylab_media_cdn_purge_urls_total{outcome="rejected"} 1`+"\n") {
		t.Fatalf("metrics:\n%s", purger.Prometheus())
	}
}

// Each batch reads the clock: a later batch's lease and backoff run from
// when it was sent, not from when the pass began.
func TestCDNPurgerTimesEachBatchFromItsOwnStart(t *testing.T) {
	t.Parallel()
	queue := media.NewMemoryCDNPurgeQueue()
	at := &clock{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	client := &slowClient{clock: at, took: 5 * time.Minute, failFrom: 2}
	purger, err := media.NewCDNPurger(media.CDNPurgerConfig{Base: "https://cdn.example.com", Queue: queue, Client: client, Now: at.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := range media.CDNPurgeBatch + 1 {
		if err := purger.QueueKey(ctx, fmt.Sprintf("images/%02d.jpg", i)); err != nil {
			t.Fatal(err)
		}
	}
	start := at.Now()
	if report, err := purger.Pass(ctx); err != nil || report.Purged != media.CDNPurgeBatch || report.Failed != 1 {
		t.Fatalf("report %+v, err %v", report, err)
	}
	// The second batch began five minutes in; its backoff counts from then.
	at.mu.Lock()
	at.now = start.Add(5*time.Minute + media.CDNPurgeRetryFirst/2)
	at.mu.Unlock()
	if due, _ := queue.ClaimCDNPurges(ctx, at.Now(), time.Second, 10); len(due) != 0 {
		t.Fatalf("due before its backoff from the batch's start: %v", due)
	}
}

// slowClient takes its time on each call (moving the clock) and fails from
// its failFrom-th call on.
type slowClient struct {
	clock    *clock
	took     time.Duration
	failFrom int
	calls    int
}

func (c *slowClient) PurgeURLs(context.Context, []string) error {
	c.calls++
	c.clock.advance(c.took)
	if c.calls >= c.failFrom {
		return errors.New("cloudflare down")
	}
	return nil
}

func TestCDNPurgerReportsItsLastSuccess(t *testing.T) {
	t.Parallel()
	_, client := newFakeCloudflare(t)
	purger, _, at := newPurger(t, client)
	if !strings.Contains(purger.Prometheus(), "skylab_media_cdn_purge_last_success_timestamp_seconds 0\n") {
		t.Fatalf("before any purge:\n%s", purger.Prometheus())
	}
	if err := purger.QueueKey(context.Background(), "images/a.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := purger.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("skylab_media_cdn_purge_last_success_timestamp_seconds %d\n", at.Now().Unix())
	if text := purger.Prometheus(); !strings.Contains(text, want) || !strings.Contains(text, "skylab_media_cdn_purge_enabled 1\n") {
		t.Fatalf("want %q in:\n%s", want, text)
	}
}

// Purging off, core says so on /v1/metrics, and whether its settings are
// wrong (a mistake that turns it off instead of stopping core).
func TestCDNPurgeOffMetrics(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		off  media.CDNPurgeOff
		want []string
	}{
		{media.CDNPurgeOff{}, []string{"skylab_media_cdn_purge_enabled 0\n", "skylab_media_cdn_purge_misconfigured 0\n"}},
		{media.CDNPurgeOff{Misconfigured: true}, []string{"skylab_media_cdn_purge_enabled 0\n", "skylab_media_cdn_purge_misconfigured 1\n"}},
	} {
		for _, line := range test.want {
			if !strings.Contains(test.off.Prometheus(), line) {
				t.Errorf("%+v: missing %q in:\n%s", test.off, line, test.off.Prometheus())
			}
		}
	}
}

func TestCDNPurgerPassPurgesInBatchesOfThirty(t *testing.T) {
	t.Parallel()
	fake, client := newFakeCloudflare(t)
	purger, queue, _ := newPurger(t, client)
	ctx := context.Background()
	for i := range 65 {
		if err := purger.QueueKey(ctx, fmt.Sprintf("images/%02d.jpg", i)); err != nil {
			t.Fatal(err)
		}
	}

	report, err := purger.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	calls := fake.purged()
	if len(calls) != 3 || len(calls[0]) != media.CDNPurgeBatch || len(calls[1]) != media.CDNPurgeBatch || len(calls[2]) != 5 {
		t.Fatalf("calls of %d", len(calls))
	}
	if report.Purged != 65 || report.Failed != 0 || len(queue.URLs()) != 0 {
		t.Fatalf("report %+v, left %v", report, queue.URLs())
	}
}

func TestCDNPurgerRetriesAFailedPurgeWithBackoffAndNeverLosesIt(t *testing.T) {
	t.Parallel()
	fake, client := newFakeCloudflare(t)
	purger, queue, at := newPurger(t, client)
	ctx := context.Background()
	if err := purger.QueueKey(ctx, "images/a.jpg"); err != nil {
		t.Fatal(err)
	}
	fake.set(http.StatusInternalServerError, false)

	report, err := purger.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 1 || report.Err == nil || queue.Attempts("https://cdn.example.com/images/a.jpg") != 1 {
		t.Fatalf("report %+v", report)
	}
	// Not due again at once: the backoff holds it.
	if report, _ := purger.Pass(ctx); report.Purged+report.Failed != 0 {
		t.Fatalf("retried at once: %+v", report)
	}
	at.advance(media.CDNPurgeRetryFirst)
	if report, _ := purger.Pass(ctx); report.Failed != 1 {
		t.Fatalf("second try %+v", report)
	}
	// The second failure waits twice as long.
	at.advance(media.CDNPurgeRetryFirst)
	if report, _ := purger.Pass(ctx); report.Purged+report.Failed != 0 {
		t.Fatalf("retried before its backoff: %+v", report)
	}
	fake.set(0, false)
	at.advance(media.CDNPurgeRetryFirst)
	if report, _ := purger.Pass(ctx); report.Purged != 1 || len(queue.URLs()) != 0 {
		t.Fatalf("last try %+v, left %v", report, queue.URLs())
	}
	if calls := fake.purged(); len(calls) != 3 {
		t.Fatalf("%d calls", len(calls))
	}
}

func TestCDNPurgerDropsAddressesOlderThanTheEdgeCacheKeepsAnything(t *testing.T) {
	t.Parallel()
	fake, client := newFakeCloudflare(t)
	purger, queue, at := newPurger(t, client)
	ctx := context.Background()
	if err := purger.QueueKey(ctx, "images/old.jpg"); err != nil {
		t.Fatal(err)
	}
	fake.set(http.StatusInternalServerError, false)
	if _, err := purger.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	at.advance(media.CDNPurgeGiveUpAfter + time.Second)
	if err := purger.QueueKey(ctx, "images/new.jpg"); err != nil {
		t.Fatal(err)
	}
	fake.set(0, false)

	report, err := purger.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Dropped != 1 || report.Purged != 1 || len(queue.URLs()) != 0 {
		t.Fatalf("report %+v", report)
	}
	if !strings.Contains(purger.Prometheus(), `skylab_media_cdn_purge_urls_total{outcome="dropped"} 1`) {
		t.Fatalf("metrics:\n%s", purger.Prometheus())
	}
}

func TestCDNPurgerPrometheusCountsOutcomesAndTheBacklog(t *testing.T) {
	t.Parallel()
	fake, client := newFakeCloudflare(t)
	purger, queue, at := newPurger(t, client)
	ctx := context.Background()
	for _, key := range []string{"images/a.jpg", "images/b.jpg"} {
		if err := purger.QueueKey(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := purger.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if err := purger.QueueKey(ctx, "images/c.jpg"); err != nil {
		t.Fatal(err)
	}
	fake.set(http.StatusBadGateway, false)
	at.advance(time.Minute)
	if _, err := purger.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	queue.FailEnqueue(errors.New("database down"))
	if err := purger.QueueKey(ctx, "images/d.jpg"); err == nil {
		t.Fatal("a queue failure was swallowed")
	}

	text := purger.Prometheus()
	for _, line := range []string{
		`skylab_media_cdn_purge_urls_total{outcome="purged"} 2`,
		`skylab_media_cdn_purge_urls_total{outcome="failed"} 1`,
		`skylab_media_cdn_purge_urls_total{outcome="dropped"} 0`,
		`skylab_media_cdn_purge_queue_errors_total 1`,
		`skylab_media_cdn_purge_backlog 1`,
		`skylab_media_cdn_purge_oldest_age_seconds 60`,
	} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("missing %q in:\n%s", line, text)
		}
	}
	if strings.Contains(text, "cdn.example.com") {
		t.Fatalf("the metrics name an address:\n%s", text)
	}
}

func TestCDNPurgerRunPurgesSoonAfterAKeyIsQueued(t *testing.T) {
	t.Parallel()
	fake, client := newFakeCloudflare(t)
	queue := media.NewMemoryCDNPurgeQueue()
	purger, err := media.NewCDNPurger(media.CDNPurgerConfig{Base: "https://cdn.example.com", Queue: queue, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := purger.Run(ctx, t.Logf)
	t.Cleanup(func() { cancel(); <-done })

	if err := purger.QueueKey(ctx, "images/a.jpg"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(queue.URLs()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not purged; calls %v", fake.purged())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNewCDNPurgerRefusesAMissingPart(t *testing.T) {
	t.Parallel()
	queue := media.NewMemoryCDNPurgeQueue()
	for _, config := range []media.CDNPurgerConfig{
		{Queue: queue, Client: media.CloudflarePurge{}},
		{Base: "https://cdn.example.com", Client: media.CloudflarePurge{}},
		{Base: "https://cdn.example.com", Queue: queue},
		{Base: "cdn.example.com", Queue: queue, Client: media.CloudflarePurge{}},
	} {
		if _, err := media.NewCDNPurger(config); err == nil {
			t.Fatalf("accepted %+v", config)
		}
	}
}

// keyRecorder records the keys R2 hands over for a CDN purge.
type keyRecorder struct {
	mu   sync.Mutex
	keys []string
	err  error
}

func (r *keyRecorder) QueueKey(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, key)
	return r.err
}

func TestR2_DeleteQueuesTheKeyForACDNPurgeOnceTheObjectIsGone(t *testing.T) {
	t.Parallel()
	r2, fake := fakeR2(t)
	recorder := &keyRecorder{}
	r2.PurgeCDNOnChange(recorder)
	fake.fail("/media/images/gone.jpg", "NoSuchKey")
	fake.fail("/media/images/kept.jpg", "AccessDenied")
	ctx := context.Background()

	if err := r2.Delete(ctx, "images/a.jpg"); err != nil {
		t.Fatal(err)
	}
	// An object already gone may still be at the edge: it is purged too.
	if err := r2.Delete(ctx, "images/gone.jpg"); err != nil {
		t.Fatal(err)
	}
	// A delete that failed leaves the object, so nothing is purged.
	if err := r2.Delete(ctx, "images/kept.jpg"); err == nil {
		t.Fatal("the failed delete succeeded")
	}
	if want := []string{"images/a.jpg", "images/gone.jpg"}; !slices.Equal(recorder.keys, want) {
		t.Fatalf("queued %v, want %v", recorder.keys, want)
	}
}

func TestR2_DeleteFailsWhenItsCDNPurgeCannotBeQueued(t *testing.T) {
	t.Parallel()
	r2, _ := fakeR2(t)
	r2.PurgeCDNOnChange(&keyRecorder{err: errors.New("database down")})

	// The object is gone; the error makes the caller delete it again, which
	// queues the purge again (every delete by key may be repeated).
	if err := r2.Delete(context.Background(), "images/a.jpg"); err == nil {
		t.Fatal("the lost purge was swallowed")
	}
}

func TestR2_SetMetadataQueuesTheKeyForACDNPurge(t *testing.T) {
	t.Parallel()
	r2, fake := fakeR2(t)
	recorder := &keyRecorder{}
	r2.PurgeCDNOnChange(recorder)
	fake.fail("/media/files/gone", "NoSuchKey")
	ctx := context.Background()

	if err := r2.SetMetadata(ctx, "files/club", media.BlobMetadata{ContentType: "application/octet-stream", ContentDisposition: "attachment"}); err != nil {
		t.Fatal(err)
	}
	if err := r2.SetMetadata(ctx, "files/gone", media.BlobMetadata{ContentType: "application/octet-stream"}); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"files/club"}; !slices.Equal(recorder.keys, want) {
		t.Fatalf("queued %v, want %v", recorder.keys, want)
	}
}

// A public key is written once, so a write needs no purge, but for an
// image's size: the size backfill writes it again when it runs again
// (ticket 17). A size written over one already stored is queued.
func TestR2_PutQueuesAPurgeOnlyWhenItOverwritesASize(t *testing.T) {
	t.Parallel()
	r2, bucket := multipartR2(t)
	recorder := &keyRecorder{}
	r2.PurgeCDNOnChange(recorder)
	ctx := context.Background()
	jpeg := media.BlobMetadata{ContentType: "image/jpeg"}

	for _, key := range []string{"images/a", "images/a/card.jpg", "images/a/card.jpg", "files/b", "files/b"} {
		if err := r2.Put(ctx, key, []byte("data"), jpeg); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"images/a/card.jpg"}; !slices.Equal(recorder.keys, want) {
		t.Fatalf("queued %v, want %v", recorder.keys, want)
	}
	if heads := bucket.Count("HeadObject"); heads != 5+2 {
		t.Fatalf("%d HEADs: one after every write, one before each size write", heads)
	}
}

func TestR2_WithoutACDNPurgeDeletesAsBefore(t *testing.T) {
	t.Parallel()
	r2, _ := fakeR2(t)
	if err := r2.Delete(context.Background(), "images/a.jpg"); err != nil {
		t.Fatal(err)
	}
}
