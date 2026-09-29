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

// A video's keys: the one a Direct upload stores it at (its original,
// videos/<name>.mp4, directServedKey), and each faststart copy beside it,
// one per claim that writes one: videos/<name>.fs.<claim id>.mp4. A worker
// writes only the key of its own claim, and deletes only that one, unless
// the Media points at it (FaststartCopyDisposable): no worker can delete a
// copy another worker moved the Media to. Copies a worker could not delete
// (a crash, a failed delete) are swept by prefix (videos/<name>.fs.): by the
// hour after a move and by a failed rewrite, every one but the copy the
// Media points at; by every purge of the Media, every one
// (purgeMediaObjects, purgeDeleter).
const (
	videoKeyPrefix       = "videos/"
	videoKeySuffix       = ".mp4"
	faststartCopyInfix   = ".fs."
	faststartCopyIDChars = 32
)

// videoName is the name in a video's key, videos/<name>.mp4 or
// videos/<name>.fs.<claim>.mp4, and the claim part of a copy's ("" for
// the original).
func videoName(key string) (name, claim string, ok bool) {
	rest, ok := strings.CutPrefix(key, videoKeyPrefix)
	if !ok || strings.Contains(rest, "/") {
		return "", "", false
	}
	base, ok := strings.CutSuffix(rest, videoKeySuffix)
	if !ok {
		return "", "", false
	}
	name, claim, isCopy := strings.Cut(base, faststartCopyInfix)
	switch {
	case name == "":
		return "", "", false
	case isCopy:
		// Never an original named "<name>.fs.<x>".
		return name, claim, claim != "" && !strings.Contains(claim, ".")
	}
	return name, "", true
}

// faststartCopyKey is the key the claim writes the faststart copy of the
// video at original to; ok is false for a key that is not a video's
// original.
func faststartCopyKey(original string, claim uuid.UUID) (string, bool) {
	name, copyOf, ok := videoName(original)
	if !ok || copyOf != "" {
		return "", false
	}
	id := strings.ReplaceAll(claim.String(), "-", "")
	return videoKeyPrefix + name + faststartCopyInfix + id[:faststartCopyIDChars] + videoKeySuffix, true
}

// videoOriginalOf is the original of any key of a video (itself, for the
// original); ok is false for a key that is not a video's.
func videoOriginalOf(key string) (string, bool) {
	name, _, ok := videoName(key)
	if !ok {
		return "", false
	}
	return videoKeyPrefix + name + videoKeySuffix, true
}

// faststartCopiesPrefix is the prefix every faststart copy of the video
// at key starts with; ok is false for a key that is not a video's.
func faststartCopiesPrefix(key string) (string, bool) {
	name, _, ok := videoName(key)
	if !ok {
		return "", false
	}
	return videoKeyPrefix + name + faststartCopyInfix, true
}

// isFaststartKey reports whether key is a video's faststart copy.
func isFaststartKey(key string) bool {
	_, claim, ok := videoName(key)
	return ok && claim != ""
}

// keyLister lists the keys under a prefix (R2's ListObjectsV2).
type keyLister interface {
	ListKeys(ctx context.Context, prefix string) ([]string, error)
}

// errNoListing is a store that cannot list keys (Buckets over one).
var errNoListing = errors.New("media: the store cannot list keys")

// purgeDeleter is how a purge deletes a Media's objects from blobs: each
// key it is given, and, for a video's original, every faststart copy beside
// it (listed by their prefix): the copy the Media pointed at and any a
// rewrite cut short left. purgeMediaObjects hands it a video's original
// whichever key the Media pointed at. A store that cannot list deletes the
// keys it is given alone.
func purgeDeleter(ctx context.Context, blobs BlobStore) func(key string) error {
	return func(key string) error {
		if err := blobs.Delete(ctx, key); err != nil {
			return err
		}
		prefix, ok := faststartCopiesPrefix(key)
		lister, canList := blobs.(keyLister)
		if !ok || isFaststartKey(key) || !canList {
			return nil
		}
		copies, err := lister.ListKeys(ctx, prefix)
		if errors.Is(err, errNoListing) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, copyKey := range copies {
			if isFaststartKey(copyKey) {
				if err := blobs.Delete(ctx, copyKey); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

// FaststartStorage is the public bucket as the faststart rewrite uses it
// (R2): it reads the video by ranges, writes the faststart copy (a small one
// whole, a larger one as a multipart upload whose parts storage copies
// from the video where it can), checks it, lists a video's copies, and
// deletes.
type FaststartStorage interface {
	keyLister
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

// FaststartState is where a video's rewrite is (column video_faststart);
// none while it waits for it.
type FaststartState string

const (
	// FaststartMoved: the Media points at its faststart copy, and its
	// original waits out FaststartOriginalGrace.
	FaststartMoved FaststartState = "moved"
	// FaststartDone: the copy was checked there once more, and the
	// original is deleted.
	FaststartDone FaststartState = "done"
	// FaststartNotNeeded: its moov came first already.
	FaststartNotNeeded FaststartState = "not_needed"
	// FaststartFailed: a file the rewrite cannot move safely, one it kept
	// failing on (faststartMaxAttempts), or one it panicked on. It is
	// served as it is, and plays once downloaded whole.
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
	// step that failed on storage is tried again; each failure in a row
	// doubles it. After faststartMaxAttempts a rewrite is given up (about
	// a day of trying).
	faststartRetryFirst  = time.Minute
	faststartRetryMax    = 6 * time.Hour
	faststartMaxAttempts = 12
	// faststartStorageTimeout bounds a storage call that moves little: a
	// HEAD, a delete, opening or joining a multipart upload, the reads of
	// box headers and of the chunks Verify compares.
	faststartStorageTimeout = 30 * time.Second
	// faststartDatabaseTimeout bounds a short transaction after the work.
	faststartDatabaseTimeout = 10 * time.Second
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

// errFaststartPanicked and errFaststartMovedPanicked are what the log
// says of a step that panicked: the video's id, never what the panic held.
var (
	errFaststartPanicked      = errors.New("the faststart rewrite panicked; the video is served as it is")
	errFaststartMovedPanicked = errors.New("the faststart step after the hour panicked; the video stays at its copy, and the step is tried again later")
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
//     short transaction under the claim: the Media is moved.
//   - FaststartOriginalGrace later, the copy is checked there once more and
//     the original goes: the Media is done. A copy that is not there whole
//     moves the Media back to its original, rewritten again.
//   - A step that fails on storage is tried again later, each failure in a
//     row doubling the wait; a rewrite is given up (failed) after
//     faststartMaxAttempts. A step that panics fails the video.
//   - A worker deletes a copy it wrote only when the Media does not point
//     at it, and either its claim still holds or the Media's purge has
//     begun (FaststartCopyDisposable).
//   - The archive and expiry purges wait for a live claim, and delete both
//     keys of a video whichever the Media points at (purgeMediaObjects).
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
	// Finished are the moved videos whose original was deleted: done.
	Finished int
	// MovedBack are the moved videos whose copy was not there whole: back
	// at their original, rewritten again.
	MovedBack int
	// Refused are the videos the rewrite refused, Abandoned those it gave
	// up on after faststartMaxAttempts, Panicked those it panicked on:
	// failed, served as they are.
	Refused   int
	Abandoned int
	Panicked  int
	// Retried are the videos whose step failed, tried again later.
	Retried int
	// Claimed are the videos the pass claimed.
	Claimed int
}

func (r FaststartReport) any() bool {
	return r.Rewritten+r.NotNeeded+r.Finished+r.MovedBack+r.Refused+r.Abandoned+r.Panicked+r.Retried > 0
}

// FaststartError is a video whose step failed (Err says why, never a file
// name): it is tried again later, or failed when it panicked.
type FaststartError struct {
	ID  uuid.UUID
	Err error
}

func (e *FaststartError) Error() string { return fmt.Sprintf("media %s: %v", e.ID, e.Err) }

func (e *FaststartError) Unwrap() error { return e.Err }

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
// to faststartBatch of them, and moves it on (faststartJob). A video that
// fails is reported through onError and tried again later; one the
// rewrite refuses, or panics on, is reported too.
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
		faststartJob{w: w, claim: claim, counts: &report, onError: onError}.run(ctx)
	}
	return report, nil
}

// faststartJob is one claimed video's step: the claim, the pass's counts,
// and where failures are reported.
type faststartJob struct {
	w       *FaststartWorker
	claim   FaststartClaim
	counts  *FaststartReport
	onError func(error)
}

func (j faststartJob) report(err error) {
	if j.onError != nil {
		j.onError(err)
	}
}

// run moves the claimed video on: a moved one to done (dropOriginal), any
// other to its faststart copy (rewrite). A step that fails is put off. One
// that panics fails the video, unless the video is moved already: its copy
// is served, and the step that drops its original is tried again later
// (so the original is not left until a purge), the attempt counted.
func (j faststartJob) run(ctx context.Context) {
	panicked, err := j.step(ctx)
	switch {
	case panicked && j.claim.State == FaststartMoved:
		j.counts.Panicked++
		j.report(&FaststartError{ID: j.claim.Media.ID, Err: errFaststartMovedPanicked})
		if err := j.deferStep(ctx, false); err != nil {
			j.report(&FaststartError{ID: j.claim.Media.ID, Err: err})
		}
	case panicked:
		j.failAfterPanic(ctx)
	case err != nil:
		j.report(&FaststartError{ID: j.claim.Media.ID, Err: err})
		j.putOff(ctx)
	}
}

func (j faststartJob) step(ctx context.Context) (panicked bool, err error) {
	defer func() {
		// Only the id is logged (errFaststartPanicked), never what the
		// panic held.
		if recover() != nil {
			panicked = true
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, faststartWork(j.claim.Media.Size))
	defer cancel()
	if j.claim.State == FaststartMoved {
		return false, j.dropOriginal(ctx)
	}
	return false, j.rewrite(ctx)
}

// rewrite writes the video's faststart copy to its claim's own key, checks
// it, and moves the Media there.
func (j faststartJob) rewrite(ctx context.Context) error {
	m := j.claim.Media
	target, ok := faststartCopyKey(m.Key, j.claim.ID)
	if !ok {
		return j.refuse(ctx, errors.New("its key is not a video's original (videos/<uuid>.mp4)"))
	}
	layout, err := j.w.plan(ctx, m.Key)
	switch {
	case errors.Is(err, faststart.ErrAlreadyFaststart):
		return j.finish(ctx, FaststartNotNeeded, &j.counts.NotNeeded)
	case errors.Is(err, faststart.ErrInvalid):
		return j.refuse(ctx, err)
	case err != nil:
		return err
	}
	if err := j.w.write(ctx, m, target, layout); err != nil {
		return errors.Join(err, j.dropCopy(ctx, target))
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), faststartDatabaseTimeout)
	moved, err := j.w.store.MoveToFaststart(dbCtx, j.claim, target, layout.Size, j.w.now().UTC())
	cancel()
	if err != nil {
		// Whether the Media moved is unknown: the copy stays. Should the
		// Media not have moved, a sweep or its purge takes the copy.
		return err
	}
	if !moved {
		// The claim is lost, or the Media archived or being purged.
		return errors.Join(j.dropCopy(ctx, target), j.release(ctx))
	}
	j.counts.Rewritten++
	return nil
}

// dropOriginal ends a moved video's hour: once the copy the Media points
// at is there whole (a HEAD), the original goes, then every other copy
// (sweep), and the video is done. No worker deletes the copy the Media
// points at meanwhile: it is the key of the claim that moved it, and no
// other claim deletes a key but its own. A copy that is not there whole
// moves the Media back to its original, which the next pass rewrites
// again; with the original gone too, the video is lost, and failed.
func (j faststartJob) dropOriginal(ctx context.Context) error {
	m := j.claim.Media
	original, ok := videoOriginalOf(m.Key)
	if !ok || !isFaststartKey(m.Key) {
		return j.refuse(ctx, errors.New("it is moved, but its key is not a faststart copy's (videos/<uuid>.fs.<claim>.mp4)"))
	}
	size, err := j.w.size(ctx, m.Key)
	switch {
	case err == nil && size == m.Size:
		if err := j.w.delete(ctx, original); err != nil {
			return err
		}
		if err := j.sweep(ctx); err != nil {
			return err
		}
		return j.finish(ctx, FaststartDone, &j.counts.Finished)
	case err != nil && !errors.Is(err, ErrNotFound):
		return err
	}
	originalSize, err := j.w.size(ctx, original)
	if errors.Is(err, ErrNotFound) {
		return j.refuse(ctx, errors.New("its faststart copy and its original are both gone"))
	}
	if err != nil {
		return err
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), faststartDatabaseTimeout)
	back, err := j.w.store.MoveBackFromFaststart(dbCtx, j.claim, original, originalSize, j.w.now().UTC())
	cancel()
	if err != nil || !back {
		return err
	}
	j.counts.MovedBack++
	j.report(&FaststartError{ID: m.ID, Err: errors.New("its faststart copy was not there whole; it is back at its original, to be rewritten again")})
	return nil
}

// finish ends the video's rewrite as state, counting it.
func (j faststartJob) finish(ctx context.Context, state FaststartState, count *int) error {
	done, err := j.w.store.FinishFaststart(ctx, j.claim, state)
	if done {
		*count++
	}
	return err
}

// refuse fails a video the rewrite refuses: it is served as it is. Its
// stray copies go first, while the claim holds.
func (j faststartJob) refuse(ctx context.Context, why error) error {
	j.sweepOrReport(ctx)
	done, err := j.w.store.FinishFaststart(ctx, j.claim, FaststartFailed)
	if err != nil || !done {
		return err
	}
	j.counts.Refused++
	j.report(&FaststartRefusal{ID: j.claim.Media.ID, Err: why})
	return nil
}

// dropCopy deletes the copy this job wrote at target (its claim's own key)
// unless the Media points at it (FaststartCopyDisposable). No other worker
// writes that key, and only this job moves the Media to it; a check that
// races errs toward keeping the copy, which a sweep or the Media's purge
// takes later.
func (j faststartJob) dropCopy(ctx context.Context, target string) error {
	base := context.WithoutCancel(ctx)
	dbCtx, cancel := context.WithTimeout(base, faststartDatabaseTimeout)
	disposable, err := j.w.store.FaststartCopyDisposable(dbCtx, j.claim, target)
	cancel()
	if err != nil || !disposable {
		return err
	}
	return j.w.delete(base, target)
}

// sweep deletes every faststart copy of the video but the one the Media
// points at: copies earlier claims could not delete (a crash, a failed
// delete) and a stale worker's. It lists them first, then checks that this
// job's claim is still the video's (FaststartClaimHolds): every listed copy
// was written under a claim that existed before that check, so a newer
// claim, whose copy the Media may be moved to, shows there and the sweep
// deletes nothing. A copy written after the listing is not in it.
func (j faststartJob) sweep(ctx context.Context) error {
	prefix, ok := faststartCopiesPrefix(j.claim.Media.Key)
	if !ok {
		return nil
	}
	base := context.WithoutCancel(ctx)
	listCtx, cancel := context.WithTimeout(base, faststartStorageTimeout)
	copies, err := j.w.storage.ListKeys(listCtx, prefix)
	cancel()
	if err != nil {
		return fmt.Errorf("list the faststart copies: %w", err)
	}
	if len(copies) == 0 {
		return nil
	}
	dbCtx, cancel := context.WithTimeout(base, faststartDatabaseTimeout)
	pointed, holds, err := j.w.store.FaststartClaimHolds(dbCtx, j.claim)
	cancel()
	if err != nil || !holds {
		return err
	}
	for _, key := range copies {
		if key == pointed || !isFaststartKey(key) {
			continue
		}
		if err := j.w.delete(base, key); err != nil {
			return err
		}
	}
	return nil
}

// sweepOrReport sweeps before the video fails; a sweep that fails is
// reported, and its copies are left to the Media's purge.
func (j faststartJob) sweepOrReport(ctx context.Context) {
	if err := j.sweep(ctx); err != nil {
		j.report(&FaststartError{ID: j.claim.Media.ID, Err: err})
	}
}

// release lets go of the claim, counting nothing against the video.
func (j faststartJob) release(ctx context.Context) error {
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), faststartDatabaseTimeout)
	defer cancel()
	return j.w.store.ReleaseFaststart(dbCtx, j.claim)
}

// putOff puts off a video whose step failed. Only a rewrite is given up
// on (its stray copies swept first): a moved video keeps trying to drop
// its original.
func (j faststartJob) putOff(ctx context.Context) {
	giveUp := j.claim.Attempts+1 >= faststartMaxAttempts && j.claim.State != FaststartMoved
	if giveUp {
		j.sweepOrReport(ctx)
	}
	if err := j.deferStep(ctx, giveUp); err != nil {
		j.report(&FaststartError{ID: j.claim.Media.ID, Err: err})
		return
	}
	if giveUp {
		j.counts.Abandoned++
	} else {
		j.counts.Retried++
	}
}

func (j faststartJob) deferStep(ctx context.Context, giveUp bool) error {
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), faststartDatabaseTimeout)
	defer cancel()
	return j.w.store.DeferFaststart(dbCtx, j.claim, j.w.now().UTC(), giveUp)
}

// failAfterPanic fails a video its rewrite panicked on: served as it is,
// never tried again, the attempt counted. Its copies (one the step may
// have written) go first, while the claim holds.
func (j faststartJob) failAfterPanic(ctx context.Context) {
	sweepErr := j.sweep(ctx)
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), faststartDatabaseTimeout)
	defer cancel()
	j.counts.Panicked++
	j.report(&FaststartError{ID: j.claim.Media.ID, Err: errors.Join(errFaststartPanicked, sweepErr, j.w.store.FailFaststart(dbCtx, j.claim))})
}

// plan plans the rewrite of the video at key, from its size and the boxes
// it reads by ranges.
func (w *FaststartWorker) plan(ctx context.Context, key string) (faststart.Layout, error) {
	size, err := w.size(ctx, key)
	if err != nil {
		return faststart.Layout{}, fmt.Errorf("size the video: %w", err)
	}
	planCtx, cancel := context.WithTimeout(ctx, faststartReadTimeout(w.limits.MaxMoovBytes)+faststartStorageTimeout)
	defer cancel()
	return faststart.Plan(planCtx, objectRanges{storage: w.storage, key: key}, size, w.limits)
}

// write writes the faststart copy to target, its claim's own key (nothing
// is there before), and checks it. It deletes nothing first: no delete of
// a worker's can land on a key another worker moved the Media to.
func (w *FaststartWorker) write(ctx context.Context, m Media, target string, layout faststart.Layout) error {
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

// partSizeFor is the part size of a copy of size bytes: the worker's, or,
// were that to take more than maxMultipartParts parts, the fewest whole
// MiB that do not.
func (w *FaststartWorker) partSizeFor(size int64) int64 {
	const mib = 1 << 20
	least := ceilDiv(size, maxMultipartParts)
	return max(w.partSize, ceilDiv(least, mib)*mib)
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// verify checks the stored copy before anything points at it: its size,
// and faststart.Verify.
func (w *FaststartWorker) verify(ctx context.Context, source, target string, layout faststart.Layout) error {
	size, err := w.size(ctx, target)
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

func (w *FaststartWorker) size(ctx context.Context, key string) (int64, error) {
	headCtx, cancel := context.WithTimeout(ctx, faststartStorageTimeout)
	defer cancel()
	return w.storage.Size(headCtx, key)
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
// with nothing to do says nothing. A pass that panics outside a video's
// step is logged, and the next one comes as usual.
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
			timer.Reset(w.runPass(ctx, logf))
		}
	}()
}

// runPass makes one pass for Run and says when the next one comes.
func (w *FaststartWorker) runPass(ctx context.Context, logf func(format string, args ...any)) (next time.Duration) {
	next = faststartPollInterval
	defer func() {
		if recover() != nil {
			logf("media faststart: a pass panicked; the next one comes as usual")
		}
	}()
	report, err := w.Pass(ctx, func(err error) { logf("media faststart: %v", err) })
	if err != nil && ctx.Err() == nil {
		logf("media faststart: %v", err)
	}
	if report.any() {
		logf("media faststart: %d rewritten, %d already faststart, %d originals deleted, %d moved back, %d refused, %d given up and %d panicked on (served as they are), %d failed (tried again later)",
			report.Rewritten, report.NotNeeded, report.Finished, report.MovedBack, report.Refused, report.Abandoned, report.Panicked, report.Retried)
	}
	if report.Claimed == faststartBatch {
		next = 0
	}
	return next
}
