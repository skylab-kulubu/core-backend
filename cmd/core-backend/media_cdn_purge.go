package main

import (
	"context"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// startCDNPurge wires the CDN cache purge (MEDIA_CDN_PURGE_*,
// docs/media-lifecycle.md "CDN cache"): every object the public bucket
// deletes, or gives new metadata, is purged from Cloudflare's cache through
// a queue on the purge's own database connections. It returns the purger
// (nil when off), what /v1/metrics shows of it, and a channel that closes
// once the worker has stopped after ctx was cancelled and its connections
// are closed (at once when the purge is off); shutdown waits on it.
//
// The purge is not what core is for, so settings it cannot use turn it off
// with a loud startup line and skylab_media_cdn_purge_misconfigured 1,
// instead of stopping core. Unset, or without R2, it is off as well.
func startCDNPurge(ctx context.Context, getenv func(string) string, publicBlobs media.BlobStore, cdnBase string, logf func(string, ...any)) (*media.CDNPurger, interface{ Prometheus() string }, <-chan struct{}) {
	off := make(chan struct{})
	close(off)
	wrong := func(err error) (*media.CDNPurger, interface{ Prometheus() string }, <-chan struct{}) {
		logf("media CDN purge: OFF, settings are wrong: %v; deleted media stays at the CDN until its copies run out", err)
		return nil, media.CDNPurgeOff{Misconfigured: true}, off
	}
	config, err := media.CDNPurgeConfigFromEnv(getenv)
	if err != nil {
		return wrong(err)
	}
	if !config.Enabled() {
		logf("media CDN purge: off (%s is not set); the CDN keeps deleted objects until its copies run out", media.CDNPurgeZoneEnv)
		return nil, media.CDNPurgeOff{}, off
	}
	r2, ok := publicBlobs.(*media.R2)
	if !ok {
		logf("media CDN purge: off (no R2 configured)")
		return nil, media.CDNPurgeOff{}, off
	}
	pool, err := media.NewCDNPurgeQueuePool(ctx, getenv("DATABASE_URL"))
	if err != nil {
		return wrong(err)
	}
	purger, err := media.NewCDNPurger(media.CDNPurgerConfig{
		Base: cdnBase, Queue: media.NewPostgresStore(pool), Client: media.NewCloudflarePurge(config),
	})
	if err != nil {
		pool.Close()
		return wrong(err)
	}
	// Set before any worker deletes.
	r2.PurgeCDNOnChange(purger)
	done := purger.Run(ctx, logf)
	logf("media CDN purge: on (zone %s, addresses under %s)", config.ZoneID, cdnBase)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-done
		pool.Close()
	}()
	return purger, purger, stopped
}
