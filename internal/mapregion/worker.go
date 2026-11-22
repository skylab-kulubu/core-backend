package mapregion

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/mapimage"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// The worker's settings.
const (
	// MaxBytesEnv bounds a region's extract (bytes); DefaultMaxBytes is
	// its default, 2 GiB (Türkiye to zoom 15 is about 1.6 GB).
	MaxBytesEnv     = "MAP_REGION_MAX_BYTES"
	DefaultMaxBytes = 2 << 30
	// BuildsURLEnv is the Protomaps builds list (DefaultBuildsURL).
	BuildsURLEnv = "MAP_SOURCE_BUILDS_URL"

	pollInterval    = time.Minute
	lease           = 30 * time.Minute
	databaseTimeout = 10 * time.Second
)

// MaxBytesFromEnv reads MaxBytesEnv: empty is DefaultMaxBytes.
func MaxBytesFromEnv(getenv func(string) string) (int64, error) {
	raw := strings.TrimSpace(getenv(MaxBytesEnv))
	if raw == "" {
		return DefaultMaxBytes, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1<<20 {
		return 0, fmt.Errorf("%s: want a byte count of at least 1 MiB", MaxBytesEnv)
	}
	return n, nil
}

// LatestBuild is the build a region is extracted from.
type LatestBuild interface {
	Latest(ctx context.Context) (Build, error)
}

// Worker extracts the requested regions, one at a time: it claims the
// oldest (or one whose worker stopped), finds the latest Protomaps build,
// plans the extract from the build's directories (its size checked against
// the limit before any tile data is read), streams it into a multipart
// upload under a new key, and marks it ready. A failure marks the region
// failed with why; an ADMIN asks again.
type Worker struct {
	Store    *Store
	Uploader Uploader
	Builds   LatestBuild
	// Source opens a build's archive; nil reads it over HTTP.
	Source   func(url string) mapimage.RangeSource
	MaxBytes int64
	Now      func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) source(url string) mapimage.RangeSource {
	if w.Source != nil {
		return w.Source(url)
	}
	return mapimage.HTTPSource{URL: url}
}

// Pass extracts the next region, if one waits: whether it took one, and an
// error the log should say (the region's own failure is its status).
func (w *Worker) Pass(ctx context.Context) (bool, error) {
	claim, found, err := w.Store.ClaimNext(ctx, w.now().UTC(), lease)
	if err != nil || !found {
		return false, err
	}
	code, err := w.extract(ctx, claim)
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), databaseTimeout)
	defer cancel()
	switch {
	case err == nil:
		return true, nil
	case ctx.Err() != nil:
		return true, errors.Join(err, w.Store.Release(settle, claim))
	default:
		return true, errors.Join(fmt.Errorf("region %s: %s: %w", claim.Region.ID, code, err), w.Store.Fail(settle, claim, code))
	}
}

// extract does the claimed region's work; on failure it answers the code
// the region fails with.
func (w *Worker) extract(ctx context.Context, claim Claim) (string, error) {
	r := claim.Region
	build, err := w.Builds.Latest(ctx)
	if err != nil {
		return CodeSourceUnavailable, err
	}
	src := w.source(build.URL)
	archive, err := mapimage.Open(ctx, src)
	if err != nil {
		return CodeSourceUnavailable, err
	}
	metadata, err := archive.Metadata(ctx)
	if err != nil {
		return CodeSourceUnavailable, err
	}
	if version, ok := mapimage.SchemaVersion(metadata); !ok || version == "" {
		return CodeSchemaMismatch, fmt.Errorf("%w (it says %q)", mapimage.ErrSchema, version)
	}
	plan, err := mapimage.PlanExtract(ctx, archive, r.Box, r.MaxZoom, uint64(w.MaxBytes))
	switch {
	case errors.Is(err, mapimage.ErrTooLarge):
		return CodeTooLarge, err
	case errors.Is(err, mapimage.ErrNoMap):
		return CodeEmpty, err
	case err != nil:
		return CodeSourceUnavailable, err
	}
	key := ObjectKeyFor(r, build.Date)
	if err := w.Uploader.AbortMultipartUploads(ctx, key); err != nil {
		return CodeUploadFailed, err
	}
	uploadID, err := w.Uploader.CreateMultipartWith(ctx, key, media.BlobMetadata{ContentType: "application/octet-stream"})
	if err != nil {
		return CodeUploadFailed, err
	}
	size := int64(plan.Size)
	out := &parts{ctx: ctx, uploader: w.Uploader, key: key, id: uploadID, buf: make([]byte, 0, partSize), after: func(sent int64) error {
		write, cancel := context.WithTimeout(ctx, databaseTimeout)
		defer cancel()
		mine, err := w.Store.Progress(write, claim, size, sent, w.now().UTC().Add(lease))
		if err == nil && !mine {
			err = errors.New("the claim was lost")
		}
		return err
	}}
	if err := w.progressStart(ctx, claim, size); err != nil {
		return CodeFailed, err
	}
	if err := plan.WriteTo(ctx, src, out, nil); err != nil {
		abortErr := w.Uploader.AbortMultipartUploads(context.WithoutCancel(ctx), key)
		if errors.Is(err, mapimage.ErrUnavailable) {
			return CodeSourceUnavailable, errors.Join(err, abortErr)
		}
		return CodeUploadFailed, errors.Join(err, abortErr)
	}
	if err := out.close(); err != nil {
		return CodeUploadFailed, errors.Join(err, w.Uploader.AbortMultipartUploads(context.WithoutCancel(ctx), key))
	}
	ready, cancel := context.WithTimeout(context.WithoutCancel(ctx), databaseTimeout)
	defer cancel()
	mine, err := w.Store.Ready(ready, claim, key, build.Date, size)
	if err == nil && !mine {
		err = errors.New("the claim was lost")
	}
	if err != nil {
		return CodeFailed, err
	}
	return "", nil
}

// progressStart records the extract's size before its first part.
func (w *Worker) progressStart(ctx context.Context, c Claim, size int64) error {
	write, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	mine, err := w.Store.Progress(write, c, size, 0, w.now().UTC().Add(lease))
	if err == nil && !mine {
		err = errors.New("the claim was lost")
	}
	return err
}

// Run makes passes in the background until ctx ends: once a minute, and
// right after one that extracted a region. It logs each region's outcome by
// its id and name. The channel closes once the pass in flight has settled.
func (w *Worker) Run(ctx context.Context, logf func(format string, args ...any)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			next := pollInterval
			func() {
				defer func() {
					if recover() != nil {
						logf("map regions: a pass panicked; the next one comes as usual")
					}
				}()
				took, err := w.Pass(ctx)
				if err != nil && ctx.Err() == nil {
					logf("map regions: %v", err)
				} else if took {
					logf("map regions: a region was extracted")
				}
				if took {
					next = 0
				}
			}()
			timer.Reset(next)
		}
	}()
	return done
}
