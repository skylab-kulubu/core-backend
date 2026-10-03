package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CDN cache purge (media redesign ticket 29): the CDN (Cloudflare in front of
// the public bucket) keeps a copy of what it served, so an object core
// deletes stays reachable at its address until that copy runs out. Every
// object the public bucket deletes, or whose serving metadata it replaces,
// has its address queued (R2.PurgeCDNOnChange); the purge worker
// (CDNPurger.Run) has Cloudflare purge the queued addresses, in batches,
// retrying with backoff. The queue is a database table, so a restart loses
// none. A purge never holds up a delete: Cloudflare being down only delays
// it. See "CDN cache" in docs/media-lifecycle.md.

const (
	// CDNPurgeZoneEnv is the Cloudflare zone of the CDN's host (its zone
	// id, 32 hex characters).
	CDNPurgeZoneEnv = "MEDIA_CDN_PURGE_ZONE_ID"
	// CDNPurgeTokenEnv is a Cloudflare API token allowed only Zone → Cache
	// Purge on that zone. Unset with the zone, purging is off.
	CDNPurgeTokenEnv = "MEDIA_CDN_PURGE_API_TOKEN"

	// DefaultCloudflareAPI is Cloudflare's API.
	DefaultCloudflareAPI = "https://api.cloudflare.com/client/v4"

	// CDNPurgeBatch is the most addresses one purge call names. Cloudflare
	// takes 100 per call on every plan below Enterprise (its purge limits,
	// read 2026-10-03); 30, its earlier limit, keeps a call valid whatever
	// the plan.
	CDNPurgeBatch = 30
	// CDNPurgeRetryFirst is the wait before a failed purge is tried again;
	// it doubles with each failure in a row, up to cdnPurgeRetryMax.
	CDNPurgeRetryFirst = 10 * time.Second
	cdnPurgeRetryMax   = 15 * time.Minute
	// CDNPurgeGiveUpAfter is how long an address is tried: past it, the
	// CDN's own copy has run out (its edge keeps an object about two hours,
	// see docs/media-lifecycle.md), so the address is dropped and counted.
	CDNPurgeGiveUpAfter = 48 * time.Hour
	// cdnPurgeLease is how long a pass holds the addresses it took: another
	// pass (another core) does not take them meanwhile, and a pass that
	// crashed gives them back when it runs out.
	cdnPurgeLease = 2 * time.Minute
	// cdnPurgePollInterval is how often the worker looks at the queue when
	// no delete wakes it: for retries, and for addresses another core
	// queued.
	cdnPurgePollInterval = 15 * time.Second
	// cdnPurgeMaxBatches bounds one pass.
	cdnPurgeMaxBatches  = 100
	cdnPurgeCallTimeout = 20 * time.Second
)

var cloudflareZoneID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// CDNPurgeConfig is how core reaches Cloudflare's cache purge.
type CDNPurgeConfig struct {
	ZoneID   string
	APIToken string
}

// Enabled reports whether purging is on.
func (c CDNPurgeConfig) Enabled() bool { return c.ZoneID != "" }

// CDNPurgeConfigFromEnv reads MEDIA_CDN_PURGE_ZONE_ID and
// MEDIA_CDN_PURGE_API_TOKEN: both unset turns purging off, one without the
// other stops core. No error names the token.
func CDNPurgeConfigFromEnv(getenv func(string) string) (CDNPurgeConfig, error) {
	config := CDNPurgeConfig{
		ZoneID:   strings.TrimSpace(getenv(CDNPurgeZoneEnv)),
		APIToken: strings.TrimSpace(getenv(CDNPurgeTokenEnv)),
	}
	switch {
	case config.ZoneID == "" && config.APIToken == "":
		return CDNPurgeConfig{}, nil
	case config.ZoneID == "" || config.APIToken == "":
		return CDNPurgeConfig{}, fmt.Errorf("%s and %s are required together", CDNPurgeZoneEnv, CDNPurgeTokenEnv)
	case !cloudflareZoneID.MatchString(config.ZoneID):
		return CDNPurgeConfig{}, fmt.Errorf("%s must be a Cloudflare zone id (32 hex characters)", CDNPurgeZoneEnv)
	case strings.ContainsAny(config.APIToken, " \t\r\n"):
		return CDNPurgeConfig{}, fmt.Errorf("%s must be the token alone (no spaces, no \"Bearer\")", CDNPurgeTokenEnv)
	}
	return config, nil
}

// CDNPurgeClient has the CDN drop its copies of the addresses.
type CDNPurgeClient interface {
	PurgeURLs(ctx context.Context, urls []string) error
}

// CloudflarePurge is Cloudflare's purge by URL
// (POST /zones/{zone}/purge_cache with {"files": [...]}). Purging an image's
// address also purges every Cloudflare image transformation of it
// (/cdn-cgi/image/…), so the sizes of AddressCloudflare need no call of
// their own.
type CloudflarePurge struct {
	// API is Cloudflare's API base; empty is DefaultCloudflareAPI.
	API    string
	ZoneID string
	Token  string
	// HTTP is the client calls go through; nil is one with a timeout.
	HTTP *http.Client
}

// NewCloudflarePurge is the purge configured by config.
func NewCloudflarePurge(config CDNPurgeConfig) CloudflarePurge {
	return CloudflarePurge{ZoneID: config.ZoneID, Token: config.APIToken}
}

var defaultPurgeHTTP = &http.Client{Timeout: cdnPurgeCallTimeout}

// PurgeURLs purges urls (at most CDNPurgeBatch) in one call. Its error
// names the answer's status and Cloudflare's error codes, never an address,
// a message (which may echo one) or the token.
func (c CloudflarePurge) PurgeURLs(ctx context.Context, urls []string) error {
	body, err := json.Marshal(struct {
		Files []string `json:"files"`
	}{urls})
	if err != nil {
		return err
	}
	api := strings.TrimRight(c.API, "/")
	if api == "" {
		api = DefaultCloudflareAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+"/zones/"+c.ZoneID+"/purge_cache", bytes.NewReader(body))
	if err != nil {
		return errors.New("media: CDN purge: cannot build the request")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = defaultPurgeHTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		// A transport error names the API's address, not the token or the
		// addresses purged.
		return fmt.Errorf("media: CDN purge: %w", err)
	}
	defer resp.Body.Close()
	var answer struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code int `json:"code"`
		} `json:"errors"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer)
	if resp.StatusCode == http.StatusOK && decodeErr == nil && answer.Success {
		return nil
	}
	codes := make([]string, 0, len(answer.Errors))
	for _, e := range answer.Errors {
		codes = append(codes, strconv.Itoa(e.Code))
	}
	return fmt.Errorf("media: CDN purge: Cloudflare answered %d (error codes [%s])", resp.StatusCode, strings.Join(codes, " "))
}

// CDNPurgeEntry is one queued address.
type CDNPurgeEntry struct {
	URL string
	// QueuedAt is when the address was last queued; a pass removes an
	// address only if nobody queued it again meanwhile.
	QueuedAt time.Time
	// Attempts counts the purges of it that failed in a row.
	Attempts int
}

// CDNPurgeQueue keeps the addresses waiting for a purge, one entry per
// address (PostgresStore keeps them in media_cdn_purges).
type CDNPurgeQueue interface {
	// EnqueueCDNPurges queues urls, due now. An address already queued is
	// queued afresh: due now, its failures forgotten.
	EnqueueCDNPurges(ctx context.Context, urls []string, now time.Time) error
	// ClaimCDNPurges takes up to limit due addresses, oldest due first, and
	// holds them for lease.
	ClaimCDNPurges(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]CDNPurgeEntry, error)
	// CompleteCDNPurges removes the purged entries, unless one was queued
	// again since it was claimed.
	CompleteCDNPurges(ctx context.Context, done []CDNPurgeEntry) error
	// RetryCDNPurges counts a failure on each entry and makes it due at
	// at[i].
	RetryCDNPurges(ctx context.Context, failed []CDNPurgeEntry, at []time.Time) error
	// DropCDNPurges removes the entries queued before queuedBefore.
	DropCDNPurges(ctx context.Context, queuedBefore time.Time) (int, error)
	// CDNPurgeBacklog counts the entries and the age of the oldest.
	CDNPurgeBacklog(ctx context.Context, now time.Time) (int, time.Duration, error)
}

// CDNPurgerConfig is what a CDNPurger takes.
type CDNPurgerConfig struct {
	// Base is the CDN's public base (CDN_BASE): an object's address is
	// <Base>/<key>.
	Base   string
	Queue  CDNPurgeQueue
	Client CDNPurgeClient
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// CDNPurger queues the addresses of changed objects and purges them.
type CDNPurger struct {
	base   string
	queue  CDNPurgeQueue
	client CDNPurgeClient
	now    func() time.Time
	wake   chan struct{}

	mu          sync.Mutex
	purged      int64
	failed      int64
	dropped     int64
	queueErrors int64
	backlog     int
	oldest      time.Duration
}

// NewCDNPurger checks config.
func NewCDNPurger(config CDNPurgerConfig) (*CDNPurger, error) {
	base := strings.TrimRight(strings.TrimSpace(config.Base), "/")
	switch {
	case !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://"):
		return nil, errors.New("media: CDN purge: the CDN base must be an absolute address")
	case config.Queue == nil:
		return nil, errors.New("media: CDN purge: no queue")
	case config.Client == nil:
		return nil, errors.New("media: CDN purge: no Cloudflare client")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &CDNPurger{base: base, queue: config.Queue, client: config.Client, now: now, wake: make(chan struct{}, 1)}, nil
}

// QueueKey queues the CDN address of the public object at key and wakes the
// worker. A key that is an absolute address (Media stored before core kept
// keys) names no object of the bucket and is left alone.
func (p *CDNPurger) QueueKey(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" || isAbsoluteURL(key) {
		return nil
	}
	url := p.base + "/" + strings.TrimLeft(key, "/")
	if err := p.queue.EnqueueCDNPurges(ctx, []string{url}, p.now().UTC()); err != nil {
		p.mu.Lock()
		p.queueErrors++
		p.mu.Unlock()
		// The key is not named: the error reaches logs.
		return fmt.Errorf("media: queue a CDN purge: %w", err)
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}

// CDNPurgeReport counts one pass.
type CDNPurgeReport struct {
	Purged  int
	Failed  int
	Dropped int
	// Err is the last purge call's failure; the pass's addresses wait for
	// their backoff.
	Err error
}

// Pass drops the addresses past CDNPurgeGiveUpAfter, then purges the due
// ones in batches of CDNPurgeBatch until none is due. A failed call puts
// its batch back with backoff and ends the pass. Its error is the queue's
// (the database's); Cloudflare's is in the report.
func (p *CDNPurger) Pass(ctx context.Context) (CDNPurgeReport, error) {
	var report CDNPurgeReport
	now := p.now().UTC()
	dropped, err := p.queue.DropCDNPurges(ctx, now.Add(-CDNPurgeGiveUpAfter))
	if err != nil {
		return report, err
	}
	report.Dropped = dropped
	p.count(func() { p.dropped += int64(dropped) })
	for range cdnPurgeMaxBatches {
		entries, err := p.queue.ClaimCDNPurges(ctx, now, cdnPurgeLease, CDNPurgeBatch)
		if err != nil {
			return report, err
		}
		if len(entries) == 0 {
			break
		}
		urls := make([]string, len(entries))
		for i, e := range entries {
			urls[i] = e.URL
		}
		callCtx, cancel := context.WithTimeout(ctx, cdnPurgeCallTimeout)
		purgeErr := p.client.PurgeURLs(callCtx, urls)
		cancel()
		if purgeErr != nil {
			at := make([]time.Time, len(entries))
			for i, e := range entries {
				at[i] = now.Add(cdnPurgeBackoff(e.Attempts + 1))
			}
			if err := p.queue.RetryCDNPurges(ctx, entries, at); err != nil {
				return report, err
			}
			report.Failed += len(entries)
			report.Err = purgeErr
			p.count(func() { p.failed += int64(len(entries)) })
			break
		}
		if err := p.queue.CompleteCDNPurges(ctx, entries); err != nil {
			return report, err
		}
		report.Purged += len(entries)
		p.count(func() { p.purged += int64(len(entries)) })
		if len(entries) < CDNPurgeBatch {
			break
		}
	}
	backlog, oldest, err := p.queue.CDNPurgeBacklog(ctx, now)
	if err != nil {
		return report, err
	}
	p.count(func() { p.backlog, p.oldest = backlog, oldest })
	return report, nil
}

// cdnPurgeBackoff is the wait after the nth failure in a row.
func cdnPurgeBackoff(failures int) time.Duration {
	wait := CDNPurgeRetryFirst
	for i := 1; i < failures && wait < cdnPurgeRetryMax; i++ {
		wait *= 2
	}
	return min(wait, cdnPurgeRetryMax)
}

func (p *CDNPurger) count(update func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	update()
}

// Run makes a pass at once, whenever QueueKey queues an address, and every
// cdnPurgePollInterval. logf hears of each pass that did anything, by
// counts alone. The returned channel closes after ctx is cancelled.
func (p *CDNPurger) Run(ctx context.Context, logf func(string, ...any)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(cdnPurgePollInterval)
		defer ticker.Stop()
		for {
			report, err := p.Pass(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				logf("media CDN purge: queue: %v", err)
			case report.Err != nil:
				logf("media CDN purge: %d purged, %d failed and retried later, %d dropped: %v", report.Purged, report.Failed, report.Dropped, report.Err)
			case report.Purged+report.Dropped > 0:
				logf("media CDN purge: %d purged, %d dropped", report.Purged, report.Dropped)
			}
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
			case <-ticker.C:
			}
		}
	}()
	return done
}

// Prometheus is the purge's counters and its backlog at the last pass, in
// Prometheus' text format. No address appears.
func (p *CDNPurger) Prometheus() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out strings.Builder
	out.WriteString("# TYPE skylab_media_cdn_purge_urls_total counter\n")
	for _, outcome := range []struct {
		name  string
		value int64
	}{{"purged", p.purged}, {"failed", p.failed}, {"dropped", p.dropped}} {
		fmt.Fprintf(&out, "skylab_media_cdn_purge_urls_total{outcome=%q} %d\n", outcome.name, outcome.value)
	}
	fmt.Fprintf(&out, "# TYPE skylab_media_cdn_purge_queue_errors_total counter\nskylab_media_cdn_purge_queue_errors_total %d\n", p.queueErrors)
	fmt.Fprintf(&out, "# TYPE skylab_media_cdn_purge_backlog gauge\nskylab_media_cdn_purge_backlog %d\n", p.backlog)
	fmt.Fprintf(&out, "# TYPE skylab_media_cdn_purge_oldest_age_seconds gauge\nskylab_media_cdn_purge_oldest_age_seconds %d\n", int64(p.oldest/time.Second))
	return out.String()
}

// CDNKeyQueue queues a public object's CDN address for a purge
// (CDNPurger).
type CDNKeyQueue interface {
	QueueKey(ctx context.Context, key string) error
}
