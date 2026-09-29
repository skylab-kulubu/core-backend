package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/faststart"
)

// Video faststart (media redesign ticket 13): a video's MP4 whose moov box
// comes after its media data plays only once it has downloaded whole. The
// faststart worker (FaststartWorker) rewrites it with its moov in front
// (internal/faststart, qt-faststart's method in Go: no ffmpeg, no
// re-encode) into a new object beside it, and points the Media there. See
// "Video faststart" in docs/media-lifecycle.md.

// A video's two keys: the one a Direct upload stores it at
// (videos/<uuid>.mp4, directServedKey) and its faststart copy beside it
// (videos/<uuid>.fs.mp4). Each names the other, so every purge of the Media
// deletes both, whichever it points at (purgeMediaObjects).
const (
	videoKeyPrefix     = "videos/"
	videoKeySuffix     = ".mp4"
	faststartKeySuffix = ".fs.mp4"
)

// videoName is the name in a video's key, videos/<name>.mp4 or
// videos/<name>.fs.mp4, and whether the key is its faststart copy's.
func videoName(key string) (name string, faststart, ok bool) {
	rest, ok := strings.CutPrefix(key, videoKeyPrefix)
	if !ok || strings.Contains(rest, "/") {
		return "", false, false
	}
	if name, ok := strings.CutSuffix(rest, faststartKeySuffix); ok {
		// Never an original named "<name>.fs".
		return name, true, name != ""
	}
	if name, ok := strings.CutSuffix(rest, videoKeySuffix); ok && name != "" {
		return name, false, true
	}
	return "", false, false
}

// faststartKeyOf is the key of the faststart copy of the video at key; ok
// is false for a key that is not a video's original.
func faststartKeyOf(key string) (string, bool) {
	name, faststart, ok := videoName(key)
	if !ok || faststart {
		return "", false
	}
	return videoKeyPrefix + name + faststartKeySuffix, true
}

// faststartSourceOf is the key of the original of the faststart copy at
// key; ok is false for a key that is not a faststart copy's.
func faststartSourceOf(key string) (string, bool) {
	name, faststart, ok := videoName(key)
	if !ok || !faststart {
		return "", false
	}
	return videoKeyPrefix + name + videoKeySuffix, true
}

// isFaststartKey reports whether key is a video's faststart copy.
func isFaststartKey(key string) bool {
	_, ok := faststartSourceOf(key)
	return ok
}

// videoPairKey is the other key of a video's pair: the faststart copy of an
// original, the original of a faststart copy.
func videoPairKey(key string) (string, bool) {
	if other, ok := faststartKeyOf(key); ok {
		return other, true
	}
	return faststartSourceOf(key)
}

// FaststartStorage is the public bucket as the faststart rewrite uses it
// (R2): it reads the video by ranges, writes the faststart copy (a small one
// whole, a larger one as a multipart upload whose parts storage copies
// from the video where it can), checks it, and deletes.
type FaststartStorage interface {
	// Size is the stored object's size; ErrNotFound when there is none.
	Size(ctx context.Context, key string) (int64, error)
	// OpenRange streams the n bytes of the object from off (a ranged GET);
	// ErrNotFound when there is none.
	OpenRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error)
	Put(ctx context.Context, key string, data []byte, meta BlobMetadata) error
	// CreateMultipartWith opens a multipart upload at key, stored with meta
	// once completed.
	CreateMultipartWith(ctx context.Context, key string, meta BlobMetadata) (string, error)
	// UploadPart sends one part's bytes.
	UploadPart(ctx context.Context, key, uploadID string, number int32, data []byte) (string, error)
	// UploadPartCopy has storage copy n bytes of the object at from, from
	// off, as one part.
	UploadPartCopy(ctx context.Context, key, uploadID string, number int32, from string, off, n int64) (string, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []UploadedPart) error
	// Delete removes the object at key; at a faststart key it also aborts
	// any multipart upload still open there.
	Delete(ctx context.Context, key string) error
}

// FaststartState is how a video's rewrite ended (column video_faststart);
// none while it waits.
type FaststartState string

const (
	// FaststartDone: the Media points at its faststart copy, and its
	// original is deleted.
	FaststartDone FaststartState = "done"
	// FaststartNotNeeded: its moov came first already.
	FaststartNotNeeded FaststartState = "not_needed"
	// FaststartFailed: a file the rewrite cannot move safely, or one it
	// kept failing on (faststartMaxAttempts). It is served as it is, and
	// plays once downloaded whole.
	FaststartFailed FaststartState = "failed"
)

const (
	// FaststartOriginalGrace is how long a video's original stays after
	// its Media moves to its faststart copy: a player that loaded the old
	// address keeps playing.
	FaststartOriginalGrace = time.Hour
	// faststartPollInterval is how often the worker looks for videos when
	// no upload wakes it.
	faststartPollInterval = time.Minute
	// faststartBatch bounds the videos one pass claims; a full pass is
	// followed by another at once.
	faststartBatch = 25
	// faststartRetryFirst and faststartRetryMax bound the wait before a
	// rewrite that failed on storage is tried again; each failure in a row
	// doubles it. After faststartMaxAttempts the video is failed (about a
	// day of trying).
	faststartRetryFirst  = time.Minute
	faststartRetryMax    = 6 * time.Hour
	faststartMaxAttempts = 12
	// faststartStorageTimeout bounds a storage call that moves little: a
	// HEAD, a delete, opening or joining a multipart upload, the reads of
	// box headers and of the chunks Verify compares.
	faststartStorageTimeout = 30 * time.Second
	// faststartPartTimeout bounds writing or copying one part.
	faststartPartTimeout = 2 * time.Minute
	// faststartLeaseMargin is how long a claim's lease outlasts its work.
	faststartLeaseMargin = 2 * time.Minute
	// DefaultFaststartPartSize is the size of every part of a faststart copy
	// but the last (R2 wants them equal): a 2 GiB video is 128 parts. A copy
	// no larger is written whole.
	DefaultFaststartPartSize = 16 << 20
	// minFaststartPartSize is R2's smallest part but the last.
	minFaststartPartSize = 5 << 20
	// maxMultipartParts is S3's most parts in one upload.
	maxMultipartParts = 10000
)

// faststartReadTimeout bounds a ranged read of n bytes: the moov, a part.
func faststartReadTimeout(n int64) time.Duration {
	return faststartStorageTimeout + time.Duration(n>>20)*time.Second
}

// faststartWork bounds everything a claim does for a video of size bytes:
// five minutes, and a second more for every MiB (2 GiB: about 39 minutes).
// Storage copies most of it itself (UploadPartCopy); the work stops before
// the lease ends.
func faststartWork(size int64) time.Duration {
	return 5*time.Minute + time.Duration(size>>20)*time.Second
}

func faststartLease(m Media) time.Duration {
	return faststartWork(m.Size) + faststartLeaseMargin
}

// FaststartWorkerConfig is what the faststart worker needs.
type FaststartWorkerConfig struct {
	Store *PostgresStore
	// Storage is the public bucket, where videos are (R2).
	Storage FaststartStorage
	// Now defaults to time.Now.
	Now func() time.Time
	// PartSize is DefaultFaststartPartSize when left out; at least 5 MiB.
	PartSize int64
	// Limits are faststart.DefaultLimits when left out.
	Limits faststart.Limits
}

// FaststartWorker is the video faststart (media redesign ticket 13): it
// rewrites each video whose moov comes after its media data with its moov
// in front, into a new object beside it, and moves the Media there.
//
//   - Each video is claimed before any storage work (ClaimNextFaststart), in
//     a short transaction; no database connection is held while storage
//     works. Rolling deploys run two workers side by side: a video another
//     has claimed is left alone.
//   - The rewrite is planned from the video's box headers and its moov,
//     read by ranges (faststart.Plan). A video already faststart is
//     not_needed; one the rewrite refuses is failed, and served as it is.
//   - The faststart copy is written to videos/<uuid>.fs.mp4, never over the
//     original: a small one whole, a larger one as a multipart upload whose
//     parts storage copies from the original where they are its bytes
//     (UploadPartCopy), so the video is never downloaded. It is checked
//     (its size, and faststart.Verify) before the Media points at it, in a
//     short transaction under the claim. The original goes
//     FaststartOriginalGrace later, and the Media is done.
//   - A rewrite that fails on storage is tried again later, each failure in
//     a row doubling the wait, and failed after faststartMaxAttempts. Its
//     copy is deleted, and so is any multipart upload open at its key.
//   - The archive and expiry purges wait for a live claim, and delete both
//     keys of a video whichever the Media points at (purgeMediaObjects). A
//     copy that lands after a purge (its claim's lease ran out) is deleted
//     by the worker that wrote it.
//
// A pass walks the videos by id, at most faststartBatch of them, and a
// video that fails never holds up the others. Every step is idempotent: a
// pass cut short is made again.
type FaststartWorker struct {
	store    *PostgresStore
	storage  FaststartStorage
	now      func() time.Time
	partSize int64
	limits   faststart.Limits
	wake     chan struct{}
}

// NewFaststartWorker makes the faststart worker.
func NewFaststartWorker(config FaststartWorkerConfig) (*FaststartWorker, error) {
	w := &FaststartWorker{
		store: config.Store, storage: config.Storage, now: config.Now, partSize: config.PartSize,
		limits: config.Limits, wake: make(chan struct{}, 1),
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.partSize == 0 {
		w.partSize = DefaultFaststartPartSize
	}
	if w.partSize < minFaststartPartSize {
		return nil, fmt.Errorf("media faststart: a part of %d bytes is smaller than R2 takes (%d)", w.partSize, minFaststartPartSize)
	}
	if w.limits == (faststart.Limits{}) {
		w.limits = faststart.DefaultLimits
	}
	if w.store == nil || w.storage == nil {
		return nil, errors.New("media faststart: a store and storage are needed")
	}
	return w, nil
}

// Wake asks the worker for a pass now: a Direct upload stored a video.
func (w *FaststartWorker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// FaststartReport counts one pass.
type FaststartReport struct {
	// Rewritten are the videos moved to their faststart copy.
	Rewritten int
	// NotNeeded are the videos whose moov came first already.
	NotNeeded int
	// Finished are the videos whose original was deleted: done.
	Finished int
	// Refused are the videos the rewrite refused, and Abandoned those it
	// gave up on after faststartMaxAttempts: failed, served as they are.
	Refused   int
	Abandoned int
	// Retried are the videos whose step failed, tried again later.
	Retried int
	// Claimed are the videos the pass claimed.
	Claimed int
}

func (r FaststartReport) any() bool {
	return r.Rewritten+r.NotNeeded+r.Finished+r.Refused+r.Abandoned+r.Retried > 0
}

// FaststartRefusal reports a video the rewrite refused (Err says why, by
// box types and places, never a file name): it is failed, and served as it
// is.
type FaststartRefusal struct {
	ID  uuid.UUID
	Err error
}

func (r *FaststartRefusal) Error() string {
	return fmt.Sprintf("media %s is served as it is: %v", r.ID, r.Err)
}

func (r *FaststartRefusal) Unwrap() error { return r.Err }

// Pass makes one pass over the videos due: it claims each in id order, up
// to faststartBatch of them, and moves it on. A video that fails is
// reported through onError and tried again later; one the rewrite refuses
// is reported too.
func (w *FaststartWorker) Pass(ctx context.Context, onError func(error)) (FaststartReport, error) {
	var report FaststartReport
	after := uuid.Nil
	for report.Claimed < faststartBatch {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		claim, found, err := w.store.ClaimNextFaststart(ctx, w.now().UTC(), after, faststartLease)
		if err != nil || !found {
			return report, err
		}
		report.Claimed++
		after = claim.Media.ID
		if err := w.step(ctx, claim, &report, onError); err != nil {
			if onError != nil {
				onError(&BackfillError{Kind: "media", ID: claim.Media.ID, Err: err})
			}
			w.deferRewrite(ctx, claim, &report, onError)
		}
	}
	return report, nil
}

func (w *FaststartWorker) deferRewrite(ctx context.Context, claim FaststartClaim, report *FaststartReport, onError func(error)) {
	// Only a rewrite is given up on: a video moved already keeps trying
	// to delete its original.
	giveUp := claim.Attempts+1 >= faststartMaxAttempts && !isFaststartKey(claim.Media.Key)
	deferCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), faststartStorageTimeout)
	defer cancel()
	if err := w.store.DeferFaststart(deferCtx, claim, w.now().UTC(), giveUp); err != nil {
		if onError != nil {
			onError(&BackfillError{Kind: "media", ID: claim.Media.ID, Err: err})
		}
		return
	}
	if giveUp {
		report.Abandoned++
	} else {
		report.Retried++
	}
}

// step moves one claimed video on, within its claim's work time.
func (w *FaststartWorker) step(ctx context.Context, claim FaststartClaim, report *FaststartReport, onError func(error)) error {
	ctx, cancel := context.WithTimeout(ctx, faststartWork(claim.Media.Size))
	defer cancel()
	m := claim.Media
	if original, ok := faststartSourceOf(m.Key); ok {
		// Moved already, its grace over: the original goes.
		if err := w.delete(ctx, original); err != nil {
			return err
		}
		done, err := w.store.FinishFaststart(ctx, claim, FaststartDone)
		if done {
			report.Finished++
		}
		return err
	}
	target, ok := faststartKeyOf(m.Key)
	if !ok {
		return w.refuse(ctx, claim, report, onError, errors.New("its key is not a video's (videos/<uuid>.mp4)"))
	}
	layout, err := w.plan(ctx, m.Key)
	switch {
	case errors.Is(err, faststart.ErrAlreadyFaststart):
		done, err := w.store.FinishFaststart(ctx, claim, FaststartNotNeeded)
		if done {
			report.NotNeeded++
		}
		return err
	case errors.Is(err, faststart.ErrInvalid):
		return w.refuse(ctx, claim, report, onError, err)
	case err != nil:
		return err
	}
	if err := w.rewrite(ctx, m, target, layout); err != nil {
		// A copy cut short goes, with any multipart upload open at its
		// key; nothing points at it.
		return errors.Join(err, w.delete(context.WithoutCancel(ctx), target))
	}
	moved, err := w.store.MoveToFaststart(ctx, claim, target, layout.Size, w.now().UTC())
	if err != nil {
		// Whether the Media moved is unknown: the copy stays. The next
		// claim finds the Media at one key or the other.
		return err
	}
	if !moved {
		return w.dropUnmoved(ctx, claim, target)
	}
	report.Rewritten++
	return nil
}

// refuse fails a video the rewrite refuses: it is served as it is.
func (w *FaststartWorker) refuse(ctx context.Context, claim FaststartClaim, report *FaststartReport, onError func(error), why error) error {
	done, err := w.store.FinishFaststart(ctx, claim, FaststartFailed)
	if err != nil || !done {
		return err
	}
	report.Refused++
	if onError != nil {
		onError(&FaststartRefusal{ID: claim.Media.ID, Err: why})
	}
	return nil
}

// plan plans the rewrite of the video at key, from its size and the boxes
// it reads by ranges.
func (w *FaststartWorker) plan(ctx context.Context, key string) (faststart.Layout, error) {
	headCtx, cancel := context.WithTimeout(ctx, faststartStorageTimeout)
	size, err := w.storage.Size(headCtx, key)
	cancel()
	if err != nil {
		return faststart.Layout{}, fmt.Errorf("size the video: %w", err)
	}
	planCtx, cancel := context.WithTimeout(ctx, faststartReadTimeout(w.limits.MaxMoovBytes)+faststartStorageTimeout)
	defer cancel()
	return faststart.Plan(planCtx, objectRanges{storage: w.storage, key: key}, size, w.limits)
}

// rewrite writes the faststart copy to target and checks it. A copy an
// earlier attempt left there goes first, and so does any multipart upload
// still open at the key.
func (w *FaststartWorker) rewrite(ctx context.Context, m Media, target string, layout faststart.Layout) error {
	if err := w.delete(ctx, target); err != nil {
		return err
	}
	meta := ServingMetadataFor(m.Purpose, m.Type, "")
	source := objectRanges{storage: w.storage, key: m.Key}
	partSize := w.partSizeFor(layout.Size)
	if layout.Size <= partSize {
		readCtx, cancel := context.WithTimeout(ctx, faststartReadTimeout(layout.Size))
		data, err := layout.Read(readCtx, source, 0, layout.Size)
		cancel()
		if err != nil {
			return fmt.Errorf("read the video: %w", err)
		}
		putCtx, cancel := context.WithTimeout(ctx, faststartPartTimeout)
		err = w.storage.Put(putCtx, target, data, meta)
		cancel()
		if err != nil {
			return fmt.Errorf("store the faststart copy: %w", err)
		}
	} else if err := w.writeParts(ctx, m.Key, target, layout, partSize, meta); err != nil {
		return err
	}
	return w.verify(ctx, m.Key, target, layout)
}

// writeParts writes the faststart copy as a multipart upload of parts of
// partSize (the last smaller): storage copies a part that is one run of
// the original's bytes, core writes the others (the moov and what borders
// it).
func (w *FaststartWorker) writeParts(ctx context.Context, from, target string, layout faststart.Layout, partSize int64, meta BlobMetadata) error {
	openCtx, cancel := context.WithTimeout(ctx, faststartStorageTimeout)
	uploadID, err := w.storage.CreateMultipartWith(openCtx, target, meta)
	cancel()
	if err != nil {
		return fmt.Errorf("open the faststart copy: %w", err)
	}
	source := objectRanges{storage: w.storage, key: from}
	var parts []UploadedPart
	for number, off := int32(1), int64(0); off < layout.Size; number, off = number+1, off+partSize {
		n := min(partSize, layout.Size-off)
		partCtx, cancel := context.WithTimeout(ctx, faststartReadTimeout(n)+faststartPartTimeout)
		var etag string
		if at, ok := layout.CopySource(off, n); ok {
			etag, err = w.storage.UploadPartCopy(partCtx, target, uploadID, number, from, at, n)
		} else {
			var data []byte
			if data, err = layout.Read(partCtx, source, off, n); err == nil {
				etag, err = w.storage.UploadPart(partCtx, target, uploadID, number, data)
			}
		}
		cancel()
		if err != nil {
			return fmt.Errorf("write part %d of the faststart copy: %w", number, err)
		}
		parts = append(parts, UploadedPart{Number: number, ETag: etag})
	}
	joinCtx, cancel := context.WithTimeout(ctx, directJoinTimeout)
	defer cancel()
	if err := w.storage.CompleteMultipart(joinCtx, target, uploadID, parts); err != nil {
		return fmt.Errorf("join the faststart copy: %w", err)
	}
	return nil
}

// partSizeFor is the part size for a copy of size bytes: the worker's,
// or larger when that would take more than maxMultipartParts parts.
func (w *FaststartWorker) partSizeFor(size int64) int64 {
	least := (size + maxMultipartParts - 1) / maxMultipartParts
	return max(w.partSize, (least+1<<20-1)>>20<<20)
}

// verify checks the stored copy before anything points at it: its size,
// and faststart.Verify.
func (w *FaststartWorker) verify(ctx context.Context, source, target string, layout faststart.Layout) error {
	headCtx, cancel := context.WithTimeout(ctx, faststartStorageTimeout)
	size, err := w.storage.Size(headCtx, target)
	cancel()
	if err != nil {
		return fmt.Errorf("size the faststart copy: %w", err)
	}
	if size != layout.Size {
		return fmt.Errorf("%w: %d bytes stored, %d planned", faststart.ErrMismatch, size, layout.Size)
	}
	verifyCtx, cancel := context.WithTimeout(ctx, faststartReadTimeout(w.limits.MaxMoovBytes)+faststartStorageTimeout)
	defer cancel()
	return faststart.Verify(verifyCtx, objectRanges{storage: w.storage, key: target}, objectRanges{storage: w.storage, key: source}, layout, w.limits)
}

// dropUnmoved is a checked copy's end when its Media did not move to it.
// The claim may be lost to another worker, which writes the same bytes to
// the same key and may point the Media there: the copy stays, and so it
// does for an archived Media (restored, it is rewritten again; its purge
// deletes both keys). But a Media whose purge has begun, or that is gone,
// never moves: a purge that did not wait for this claim (its lease ran out)
// may have deleted the key before the copy landed, so the copy goes.
func (w *FaststartWorker) dropUnmoved(ctx context.Context, claim FaststartClaim, target string) error {
	now, err := w.store.GetIncludingDeleted(ctx, claim.Media.ID)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return err
	case now.Key == target || (now.BlobPurgeStartedAt == nil && now.BlobPurgedAt == nil):
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), faststartStorageTimeout)
		defer cancel()
		return w.store.ReleaseFaststart(releaseCtx, claim)
	}
	return w.delete(ctx, target)
}

func (w *FaststartWorker) delete(ctx context.Context, key string) error {
	deleteCtx, cancel := context.WithTimeout(ctx, faststartStorageTimeout)
	defer cancel()
	if err := w.storage.Delete(deleteCtx, key); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// objectRanges is a stored object as the rewrite reads it.
type objectRanges struct {
	storage FaststartStorage
	key     string
}

func (o objectRanges) OpenRange(ctx context.Context, off, n int64) (io.ReadCloser, error) {
	return o.storage.OpenRange(ctx, o.key, off, n)
}

// Run makes passes in the background until ctx ends: at once, whenever a
// Direct upload stores a video, and every faststartPollInterval; right
// after a pass that claimed a full batch. It logs what a pass changed and
// each video that failed or was refused (by id; never a file name); a pass
// with nothing to do says nothing.
func (w *FaststartWorker) Run(ctx context.Context, logf func(format string, args ...any)) {
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			case <-w.wake:
			}
			report, err := w.Pass(ctx, func(err error) { logf("media faststart: %v", err) })
			if err != nil && ctx.Err() == nil {
				logf("media faststart: %v", err)
			}
			if report.any() {
				logf("media faststart: %d rewritten, %d already faststart, %d originals deleted, %d refused and %d given up (served as they are), %d failed (tried again later)",
					report.Rewritten, report.NotNeeded, report.Finished, report.Refused, report.Abandoned, report.Retried)
			}
			next := faststartPollInterval
			if report.Claimed == faststartBatch {
				next = 0
			}
			timer.Reset(next)
		}
	}()
}
