package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/media/s3test"
)

const selfTestZone = "0123456789abcdef0123456789abcdef"

// fakeEdge is the CDN in front of the bucket, with Cloudflare's purge: it
// serves an object from the bucket once (MISS), then from its copy (HIT)
// until a purge names its address; Cache-Control is what the Cache Rule
// gives browsers.
type fakeEdge struct {
	cdn, api     *httptest.Server
	mu           sync.Mutex
	copies       map[string][]byte
	cacheControl string
	purges       int
}

func newFakeEdge(t *testing.T, bucket *s3test.Server, cacheControl string) *fakeEdge {
	t.Helper()
	e := &fakeEdge{copies: map[string][]byte{}, cacheControl: cacheControl}
	e.cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.cacheControl != "" {
			w.Header().Set("Cache-Control", e.cacheControl)
		}
		if body, ok := e.copies[key]; ok {
			w.Header().Set("Cf-Cache-Status", "HIT")
			_, _ = w.Write(body)
			return
		}
		w.Header().Set("Cf-Cache-Status", "MISS")
		object, ok := bucket.Object("media", key)
		if !ok {
			http.NotFound(w, r)
			return
		}
		e.copies[key] = object.Data
		_, _ = w.Write(object.Data)
	}))
	t.Cleanup(e.cdn.Close)
	e.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Files []string `json:"files"`
		}
		if r.URL.Path != "/zones/"+selfTestZone+"/purge_cache" || json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		for _, file := range body.Files {
			delete(e.copies, strings.TrimPrefix(file, e.cdn.URL+"/"))
		}
		e.purges++
		e.mu.Unlock()
		_, _ = w.Write([]byte(`{"success":true,"errors":[]}`))
	}))
	t.Cleanup(e.api.Close)
	return e
}

// selfTestRig is the check wired as in core's container: the bucket's
// deletes queue a purge, and runningCore, when true, is core's own purge
// worker on the same queue.
func selfTestRig(t *testing.T, cacheControl string, runningCore bool) (cdnSelfTest, *fakeEdge, *s3test.Server) {
	t.Helper()
	bucket := s3test.New(t)
	edge := newFakeEdge(t, bucket, cacheControl)
	queue := media.NewMemoryCDNPurgeQueue()
	client := media.CloudflarePurge{API: edge.api.URL, ZoneID: selfTestZone, Token: "token"}
	purger, err := media.NewCDNPurger(media.CDNPurgerConfig{Base: edge.cdn.URL, Queue: queue, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	r2 := media.NewR2(media.R2Config{Endpoint: bucket.URL, AccessKey: "ak", SecretKey: "sk", Bucket: "media"})
	r2.PurgeCDNOnChange(purger)
	if runningCore {
		core, err := media.NewCDNPurger(media.CDNPurgerConfig{Base: edge.cdn.URL, Queue: queue, Client: client})
		if err != nil {
			t.Fatal(err)
		}
		// The running core looks at the queue on its own clock (every 15
		// seconds); here every 20 ms.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			for ctx.Err() == nil {
				_, _ = core.Pass(ctx)
				time.Sleep(20 * time.Millisecond)
			}
		}()
		t.Cleanup(func() { cancel(); <-done })
	}
	return cdnSelfTest{
		store: r2, purger: purger, base: edge.cdn.URL, http: &http.Client{Timeout: 5 * time.Second},
		wait: 2 * time.Second, poll: 20 * time.Millisecond, fetchPause: time.Millisecond,
	}, edge, bucket
}

func TestMediaCDNPurgeSelfTestPassesWhenTheRunningCorePurgesTheDeletedObject(t *testing.T) {
	check, edge, bucket := selfTestRig(t, "max-age=3600", true)
	var out, errOut bytes.Buffer
	if code := check.run(context.Background(), &out, &errOut); code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	for _, line := range []string{"FETCH 1: 200 MISS", "FETCH 2: 200 HIT", "CACHE-CONTROL max-age=3600", "CACHED OK", "PURGE OK", "CDN SELFTEST OK"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("missing %q in:\n%s", line, out.String())
		}
	}
	if keys := bucket.Keys("media"); len(keys) != 0 {
		t.Fatalf("left in the bucket: %v", keys)
	}
	if edge.purges == 0 {
		t.Fatal("nothing was purged")
	}
}

func TestMediaCDNPurgeSelfTestFailsWhenTheRunningCoreDoesNotPurge(t *testing.T) {
	check, _, _ := selfTestRig(t, "max-age=3600", false)
	check.wait = 100 * time.Millisecond
	var out, errOut bytes.Buffer
	if code := check.run(context.Background(), &out, &errOut); code != 1 {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	// The check purged its own object, so no copy is left at the edge.
	if !strings.Contains(out.String(), "PURGE NOT SEEN") || !strings.Contains(out.String(), "PURGE SELF") {
		t.Fatalf("out:\n%s", out.String())
	}
}

func TestMediaCDNPurgeSelfTestFailsOnTheWrongBrowserTTL(t *testing.T) {
	check, _, _ := selfTestRig(t, "public, max-age=14400", true)
	var out, errOut bytes.Buffer
	if code := check.run(context.Background(), &out, &errOut); code != 1 {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "BROWSER TTL WRONG") || !strings.Contains(out.String(), "PURGE OK") || strings.Contains(out.String(), "CDN SELFTEST OK") {
		t.Fatalf("out:\n%s", out.String())
	}
}

func TestMediaCDNPurgeSelfTestNeedsThePurgeSettings(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runMediaCDNPurgeSelfTest(nil, env(map[string]string{}), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), media.CDNPurgeZoneEnv) {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	errOut.Reset()
	settings := map[string]string{media.CDNPurgeZoneEnv: selfTestZone, media.CDNPurgeTokenEnv: "token"}
	if code := runMediaCDNPurgeSelfTest(nil, env(settings), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "R2") {
		t.Fatalf("without R2: exit %d: %s", code, errOut.String())
	}
	if strings.Contains(errOut.String(), "token") && !strings.Contains(errOut.String(), media.CDNPurgeTokenEnv) {
		t.Fatalf("the error names the token: %s", errOut.String())
	}
}

// Browsers get max-age=3600 when the header names it among others
// ("public, max-age=3600"), not when it names a longer one.
func TestMediaCDNPurgeSelfTestReadsMaxAgeAsADirective(t *testing.T) {
	for header, ok := range map[string]bool{
		"max-age=3600": true, "public, max-age=3600": true, "max-age=3600, must-revalidate": true,
		"max-age=36000": false, "public, max-age=14400": false, "s-maxage=3600": false, "": false,
	} {
		if got := hasMaxAge(header, 3600); got != ok {
			t.Errorf("hasMaxAge(%q) = %v", header, got)
		}
	}
	check, _, _ := selfTestRig(t, "public, max-age=3600", true)
	var out, errOut bytes.Buffer
	if code := check.run(context.Background(), &out, &errOut); code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
}

// A test object a check left behind (a killed check cannot delete its own)
// is deleted by the next check once it is an hour old; a newer one may be
// another check's, running now.
func TestMediaCDNPurgeSelfTestSweepsStaleTestObjects(t *testing.T) {
	check, _, bucket := selfTestRig(t, "max-age=3600", true)
	ctx := context.Background()
	stale := fmt.Sprintf("selftest/cdn-purge-%d-%s.png", time.Now().Add(-2*time.Hour).Unix(), "0b0c")
	fresh := fmt.Sprintf("selftest/cdn-purge-%d-%s.png", time.Now().Add(-time.Minute).Unix(), "0d0e")
	for _, key := range []string{stale, fresh} {
		if err := check.store.Put(ctx, key, []byte("png"), media.BlobMetadata{ContentType: "image/png"}); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if code := check.run(ctx, &out, &errOut); code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	if keys := bucket.Keys("media"); len(keys) != 1 || keys[0] != fresh {
		t.Fatalf("left %v, want only %s", keys, fresh)
	}
	if !strings.Contains(out.String(), "SWEPT 1") {
		t.Fatalf("out:\n%s", out.String())
	}
}
