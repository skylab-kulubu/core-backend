package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/media/s3test"
)

// The CDN purge is not what core is for: settings it cannot use turn it
// off, loudly and on /v1/metrics, instead of stopping core.
func TestStartCDNPurgeTurnsOffInsteadOfStoppingCore(t *testing.T) {
	bucket := s3test.New(t)
	r2 := media.NewR2(media.R2Config{Endpoint: bucket.URL, AccessKey: "ak", SecretKey: "sk", Bucket: "media"})
	good := map[string]string{
		media.CDNPurgeZoneEnv: selfTestZone, media.CDNPurgeTokenEnv: "token",
		// pgx opens connections lazily: nothing is reached here.
		"DATABASE_URL": "postgres://core@127.0.0.1:1/core",
	}
	for _, test := range []struct {
		name      string
		env       map[string]string
		blobs     media.BlobStore
		on        bool
		logged    string
		misconfig bool
	}{
		{name: "on", env: good, blobs: r2, on: true, logged: "media CDN purge: on ("},
		{name: "unset", env: map[string]string{}, blobs: r2, logged: "media CDN purge: off (" + media.CDNPurgeZoneEnv + " is not set)"},
		{name: "no R2", env: good, blobs: media.NewMemoryBlob(), logged: "media CDN purge: off (no R2 configured)"},
		{name: "token missing", env: map[string]string{media.CDNPurgeZoneEnv: selfTestZone}, blobs: r2, logged: "media CDN purge: OFF, settings are wrong", misconfig: true},
		{name: "zone not an id", env: map[string]string{media.CDNPurgeZoneEnv: "yildizskylab.com", media.CDNPurgeTokenEnv: "token"}, blobs: r2, logged: "media CDN purge: OFF, settings are wrong", misconfig: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The worker logs from its own goroutine.
			var mu sync.Mutex
			var logs strings.Builder
			logf := func(format string, a ...any) {
				mu.Lock()
				defer mu.Unlock()
				fmt.Fprintf(&logs, format+"\n", a...)
			}
			ctx, cancel := context.WithCancel(context.Background())
			purger, metrics, stopped := startCDNPurge(ctx, env(test.env), test.blobs, "https://cdn.example.com", logf)
			cancel()
			// Shutdown waits on it: it closes once the worker has stopped
			// and its connections are closed, at once when off.
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("the purge did not stop after cancel")
			}
			mu.Lock()
			defer mu.Unlock()
			if (purger != nil) != test.on || !strings.Contains(logs.String(), test.logged) {
				t.Fatalf("purger %v, logs:\n%s", purger != nil, logs.String())
			}
			text := metrics.Prometheus()
			wantEnabled := map[bool]string{true: "skylab_media_cdn_purge_enabled 1\n", false: "skylab_media_cdn_purge_enabled 0\n"}[test.on]
			wantMisconfig := map[bool]string{true: "skylab_media_cdn_purge_misconfigured 1\n", false: "skylab_media_cdn_purge_misconfigured 0\n"}[test.misconfig]
			if !strings.Contains(text, wantEnabled) || !strings.Contains(text, wantMisconfig) {
				t.Fatalf("metrics:\n%s", text)
			}
			if strings.Contains(logs.String(), "token\n") || strings.Contains(logs.String(), "=token") {
				t.Fatalf("the log names the token:\n%s", logs.String())
			}
		})
	}
}
