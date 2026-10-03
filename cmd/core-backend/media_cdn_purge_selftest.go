package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// mediaCDNPurgeSelfTestCommandName checks the CDN cache and its purge
// instead of running the server: `core-backend media-cdn-purge-selftest
// [-wait 90s]` (media redesign ticket 29). Inside core's container it
// stores a 1x1 PNG in the public bucket at a fresh key
// (selftest/cdn-purge-<unix time>-<uuid>.png), fetches it from the CDN until the CDN
// answers it from its cache (cf-cache-status HIT) and reads the
// Cache-Control browsers get, then deletes it the way core deletes every
// object: through the public bucket, which queues its address in the
// database. The running core's purge worker takes it from there (it looks
// at the queue every 15 seconds); the check waits for the CDN to answer
// 404. It exits 0 only when the CDN cached the object, browsers get
// max-age=3600, and the running core's purge made the CDN drop it within
// -wait. If the running core did not purge it in time, the check purges
// it itself, so no copy is left behind, and exits 1. The deploy check runs
// it (ops/wizards/cdn-cache-purge-wizard.sh in sky_lab_genel). It prints
// the test object's address, which is an id and no one's data.
//
// SIGTERM or SIGINT ends it early, deleting its object on the way out. A
// check killed harder leaves its object behind; the next check deletes
// every test object older than an hour (its key carries its time), and
// leaves newer ones to the check that may still be running.
//
// At its longest it runs 6 fetches of at most 16 s, -wait, a purge of its
// own and 30 s more, about -wait + 3 min; the wizard allows 6 min.
const mediaCDNPurgeSelfTestCommandName = "media-cdn-purge-selftest"

// wantBrowserMaxAge is the max-age the Cache Rule gives browsers.
const wantBrowserMaxAge = 3600

// selfTestPrefix is where the check stores its test objects; staleSelfTest
// is the age past which a test object is a dead check's.
const (
	selfTestPrefix = "selftest/cdn-purge-"
	staleSelfTest  = time.Hour
)

func runMediaCDNPurgeSelfTest(args []string, getenv func(string) string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet(mediaCDNPurgeSelfTestCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	wait := flags.Duration("wait", 90*time.Second, "how long the running core's purge may take")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(errOut, "%s: %s\n", mediaCDNPurgeSelfTestCommandName, fmt.Sprintf(format, a...))
		return code
	}
	config, err := media.CDNPurgeConfigFromEnv(getenv)
	if err != nil {
		return fail(2, "%v", err)
	}
	if !config.Enabled() {
		return fail(2, "set %s and %s: the CDN purge is off", media.CDNPurgeZoneEnv, media.CDNPurgeTokenEnv)
	}
	blobs, cdnBase, err := media.BlobAndCDN(getenv)
	if err != nil {
		return fail(2, "%v", err)
	}
	r2, ok := blobs.(*media.R2)
	if !ok {
		return fail(2, "core has no R2 (R2_ENDPOINT, R2_BUCKET, R2_ACCESS_KEY, R2_SECRET_KEY)")
	}
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		return fail(2, "%s needs DATABASE_URL (the purge queue)", mediaCDNPurgeSelfTestCommandName)
	}
	signalled, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalled, *wait+3*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		return fail(2, "database: cannot open the connection pool")
	}
	defer pool.Close()
	purger, err := media.NewCDNPurger(media.CDNPurgerConfig{
		Base: cdnBase, Queue: media.NewPostgresStore(pool), Client: media.NewCloudflarePurge(config),
	})
	if err != nil {
		return fail(2, "%v", err)
	}
	r2.PurgeCDNOnChange(purger)
	check := cdnSelfTest{
		store: r2, purger: purger, base: cdnBase, http: &http.Client{Timeout: 15 * time.Second},
		wait: *wait, poll: 2 * time.Second, fetchPause: time.Second,
	}
	return check.run(ctx, out, errOut)
}

// cdnSelfTest is the check's parts, apart so a test can give fakes.
type cdnSelfTest struct {
	// store is the public bucket, its deletes queued for a purge.
	store interface {
		Put(ctx context.Context, key string, data []byte, meta media.BlobMetadata) error
		Delete(ctx context.Context, key string) error
		ListKeys(ctx context.Context, prefix string) ([]string, error)
	}
	// purger purges what the running core has not, so no copy is left.
	purger *media.CDNPurger
	base   string
	http   *http.Client
	// wait bounds the running core's purge; poll is how often the CDN is
	// asked meanwhile; fetchPause separates the fetches before the delete.
	wait, poll, fetchPause time.Duration
}

func (c cdnSelfTest) run(ctx context.Context, out, errOut io.Writer) int {
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	c.sweep(ctx, say)
	key := fmt.Sprintf("%s%d-%s.png", selfTestPrefix, time.Now().Unix(), uuid.NewString())
	address := strings.TrimRight(c.base, "/") + "/" + key
	sample, err := onePixelPNG()
	if err != nil {
		fmt.Fprintf(errOut, "%s: make the test image: %v\n", mediaCDNPurgeSelfTestCommandName, err)
		return 1
	}
	if err := c.store.Put(ctx, key, sample, media.ServingMetadata("image/png", "")); err != nil {
		fmt.Fprintf(errOut, "%s: store the test image in R2: %v\n", mediaCDNPurgeSelfTestCommandName, err)
		return 1
	}
	deleted := false
	defer func() {
		if deleted {
			return
		}
		deleteCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.store.Delete(deleteCtx, key); err != nil {
			fmt.Fprintf(errOut, "%s: delete the test image (%s): %v\n", mediaCDNPurgeSelfTestCommandName, key, err)
		}
	}()
	say("TEST OBJECT %s", address)

	// The CDN caches it: a fetch answered from the cache, and what
	// browsers are told.
	cached, cacheControl := false, ""
	for i := 1; i <= 6 && !cached; i++ {
		if i > 1 {
			pauseFor(ctx, c.fetchPause)
		}
		status, cacheStatus, control, err := c.fetch(ctx, address)
		if err != nil {
			say("FETCH %d: %v", i, err)
			continue
		}
		say("FETCH %d: %d %s", i, status, orDash(cacheStatus))
		if status == http.StatusOK {
			cacheControl = control
			cached = strings.EqualFold(cacheStatus, "HIT")
		}
	}
	say("CACHE-CONTROL %s", orDash(cacheControl))
	browserOK := hasMaxAge(cacheControl, wantBrowserMaxAge)
	if !browserOK {
		say("BROWSER TTL WRONG: browsers get %q, the Cache Rule gives max-age=%d", cacheControl, wantBrowserMaxAge)
	}
	if !cached {
		say("NOT CACHED: the CDN never answered it from its cache; the purge cannot be checked")
		return 1
	}
	say("CACHED OK")

	// Delete it as core does; the running core's worker purges it.
	deletedAt := time.Now()
	if err := c.store.Delete(ctx, key); err != nil {
		fmt.Fprintf(errOut, "%s: delete the test image: %v\n", mediaCDNPurgeSelfTestCommandName, err)
		return 1
	}
	deleted = true
	say("DELETED (its purge is queued)")
	if gone := c.waitGone(ctx, address, c.wait); gone {
		say("GONE AFTER %ds", int(time.Since(deletedAt).Seconds()))
		say("PURGE OK")
		if !browserOK {
			return 1
		}
		say("CDN SELFTEST OK")
		return 0
	}
	say("PURGE NOT SEEN: the CDN still serves it %ds after the delete; is the running core's CDN purge on?", int(time.Since(deletedAt).Seconds()))
	if report, err := c.purger.Pass(ctx); err != nil || report.Err != nil {
		say("SELF PURGE FAILED: queue %v, Cloudflare %v", err, report.Err)
		return 1
	}
	if c.waitGone(ctx, address, 30*time.Second) {
		say("PURGE SELF: this check's own purge took it; the running core did not")
	} else {
		say("PURGE FAIL: the CDN still serves it after this check's own purge")
	}
	return 1
}

// sweep deletes the test objects older than staleSelfTest, through the
// bucket, so each one's purge is queued too.
func (c cdnSelfTest) sweep(ctx context.Context, say func(string, ...any)) {
	keys, err := c.store.ListKeys(ctx, selfTestPrefix)
	if err != nil {
		say("SWEEP SKIPPED: %v", err)
		return
	}
	swept := 0
	for _, key := range keys {
		stamp, _, ok := strings.Cut(strings.TrimPrefix(key, selfTestPrefix), "-")
		unix, err := strconv.ParseInt(stamp, 10, 64)
		if !ok || err != nil || time.Since(time.Unix(unix, 0)) < staleSelfTest {
			continue
		}
		if err := c.store.Delete(ctx, key); err != nil {
			say("SWEEP: %s stays: %v", key, err)
			continue
		}
		swept++
	}
	if swept > 0 {
		say("SWEPT %d stale test objects", swept)
	}
}

// hasMaxAge reports whether the Cache-Control header gives browsers
// exactly max-age=seconds among its directives.
func hasMaxAge(header string, seconds int) bool {
	for _, directive := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(directive), "max-age="+strconv.Itoa(seconds)) {
			return true
		}
	}
	return false
}

// waitGone asks the CDN for address every poll until it answers 404, for
// at most wait.
func (c cdnSelfTest) waitGone(ctx context.Context, address string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		status, _, _, err := c.fetch(ctx, address)
		if err == nil && status == http.StatusNotFound {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		pauseFor(ctx, c.poll)
	}
}

// fetch GETs address, as a browser would, and reads the answer's status,
// cf-cache-status and Cache-Control.
func (c cdnSelfTest) fetch(ctx context.Context, address string) (int, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, "", "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header.Get("Cf-Cache-Status"), resp.Header.Get("Cache-Control"), nil
}

func pauseFor(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func onePixelPNG() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0x1f, G: 0x6f, B: 0xeb, A: 0xff})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
