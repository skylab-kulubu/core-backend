package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/mediaframe"
)

// Video frames (media redesign ticket 25, decision P1): an Event video with
// no poster its organizers uploaded gets one frame of itself as its poster.
// The frame service (cmd/media-frame, internal/mediaframe) takes it by
// ffmpeg from a short-lived presigned GET of the video; core runs it through
// the image pipeline and keeps it as a video_frame Media in the video's own
// column (event_videos.frame_media_id), never in poster_media_id. See "Video
// frames" in docs/media-lifecycle.md.

// FrameAddrEnv names the frame service's address, host:port on the internal
// network. Unset, videos get no frame.
const FrameAddrEnv = "MEDIA_FRAME_ADDR"

// FrameAt is where in a video core takes its frame: a second in, past a
// fade from black. The service takes a shorter clip's first frame.
const FrameAt = time.Second

// FrameMaxAttempts is how many failed tries a video's frame gets (about a
// day of trying) before it is given up: the video then has no poster.
const FrameMaxAttempts = 12

const (
	// framePollInterval is how often the worker looks for videos.
	framePollInterval = time.Minute
	// frameBatch bounds the videos one pass claims; a full pass is followed
	// by another at once.
	frameBatch = 25
	// frameRetryFirst and frameRetryMax bound the wait before a failed
	// frame is tried again; each failure in a row doubles it.
	frameRetryFirst = time.Minute
	frameRetryMax   = 6 * time.Hour
	// frameURLTTL is how long the presigned GET the frame service reads
	// the video by works: its queue and ffmpeg's run, with room.
	frameURLTTL = 2 * time.Minute
	// frameRequestTimeout bounds the frame service's answer: its queue
	// (30 s), ffmpeg (30 s, twice for a short clip) and the transfer.
	frameRequestTimeout = 2 * time.Minute
	// frameWork bounds everything a claim does: the frame, the image
	// pipeline, the objects and the short transactions.
	frameWork = frameRequestTimeout + time.Minute
	// frameLease is how long a claim holds: its work plus a margin.
	frameLease = frameWork + 2*time.Minute
	// frameStorageTimeout bounds one object write.
	frameStorageTimeout = 30 * time.Second
	// frameDatabaseTimeout bounds a short transaction.
	frameDatabaseTimeout = 10 * time.Second
	// frameServiceDownFirst and frameServiceDownMax bound the wait before
	// the next pass while the frame service cannot be asked.
	frameServiceDownFirst = 10 * time.Second
	frameServiceDownMax   = 5 * time.Minute
)

// ErrFrameServiceDown is a pass that stopped because the frame service
// cannot be reached, is busy, or refuses core's addresses (its allowlist
// does not name the storage): every video waits, and no try is counted.
var ErrFrameServiceDown = errors.New("media: the frame service cannot be asked")

// errFramePanicked is what the log says of a step that panicked: the
// video's id, never what the panic held.
var errFramePanicked = errors.New("the frame step panicked; the video gets no frame")

// FrameSource is the frame service as the worker asks it
// (mediaframe.Client): the JPEG frame of the video at videoURL at at. A
// video it takes no frame from is mediaframe.ErrNoFrame; a service that
// cannot be asked, mediaframe.ErrUnavailable.
type FrameSource interface {
	Frame(ctx context.Context, videoURL string, at time.Duration) ([]byte, error)
}

// FrameStorage is the public bucket as the frame worker uses it (R2): a
// presigned GET of the video for the frame service, and the frame's
// objects.
type FrameStorage interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Put(ctx context.Context, key string, data []byte, meta BlobMetadata) error
}

// FrameWorkerConfig is what the frame worker needs.
type FrameWorkerConfig struct {
	Store   *PostgresStore
	Storage FrameStorage
	Frames  FrameSource
	// Catalogue is the reviewed catalogue when left out; it must hold
	// PurposeVideoFrame.
	Catalogue Catalogue
	// DecodeBudget is the process's; nil makes one for this worker alone.
	DecodeBudget *DecodeBudget
	// Now defaults to time.Now.
	Now func() time.Time
}

// FrameWorker takes the frame of every Event video that needs one (media
// redesign ticket 25):
//
//   - Each video is claimed before the frame service is asked
//     (ClaimNextFrame), in a short transaction; no database connection is
//     held while the service or the storage works. Rolling deploys run two
//     workers side by side: a video another has claimed is left alone.
//   - The service reads the video by a presigned GET (frameURLTTL) and
//     answers one JPEG. It goes through the image pipeline within the
//     decode budget (re-encoded, with its sizes, never SVG): purposeFile
//     for PurposeVideoFrame.
//   - Its record is written first (pending, expiring, with no uploader),
//     then its objects, then the link, under the Event's lock
//     (AttachFrame). A frame whose video went meanwhile is left to expire
//     at once; the expiry cleanup deletes it.
//   - A frame that fails is tried again later, each failure in a row
//     doubling the wait, and given up after FrameMaxAttempts. A video the
//     service takes no frame from is given up at once, as is one whose step
//     panicked. A service that cannot be asked costs no video a try: the
//     pass ends (ErrFrameServiceDown) and Run waits before the next.
//
// A pass walks the videos by Event and video, at most frameBatch of them;
// a video that fails never holds up the others. Every step is idempotent:
// a pass cut short is made again, and a record it left expires.
type FrameWorker struct {
	store    *PostgresStore
	storage  FrameStorage
	frames   FrameSource
	purpose  Purpose
	decoding *DecodeBudget
	now      func() time.Time
}

// NewFrameWorker makes the frame worker.
func NewFrameWorker(config FrameWorkerConfig) (*FrameWorker, error) {
	if config.Store == nil || config.Storage == nil || config.Frames == nil {
		return nil, errors.New("media frames: a store, storage and the frame service are needed")
	}
	catalogue := config.Catalogue
	if catalogue.purposes == nil {
		catalogue = reviewedCatalogue()
	}
	purpose, ok := catalogue.Lookup(PurposeVideoFrame)
	if !ok {
		return nil, fmt.Errorf("media frames: the catalogue has no %s purpose", PurposeVideoFrame)
	}
	w := &FrameWorker{
		store: config.Store, storage: config.Storage, frames: config.Frames, purpose: purpose,
		decoding: config.DecodeBudget, now: config.Now,
	}
	if w.decoding == nil {
		w.decoding = NewDecodeBudget(DecodeBudgetConfig{})
	}
	if w.now == nil {
		w.now = time.Now
	}
	return w, nil
}

// FrameReport counts one pass.
type FrameReport struct {
	// Made are the frames stored and linked to their video.
	Made int
	// Failed are the videos the service takes no frame from; Abandoned
	// those given up after FrameMaxAttempts; Panicked those whose step
	// panicked. None of them gets a frame.
	Failed    int
	Abandoned int
	Panicked  int
	// Retried are the videos whose frame failed, tried again later.
	Retried int
	// Released are the claims let go without a try counted: the service
	// could not be asked, or the decode budget was busy.
	Released int
	// Discarded are the frames made for a video that left the Event
	// meanwhile: left to expire at once.
	Discarded int
	// Claimed are the videos the pass claimed.
	Claimed int
}

func (r FrameReport) any() bool {
	return r.Made+r.Failed+r.Abandoned+r.Panicked+r.Retried+r.Discarded > 0
}

// FrameError is a video whose frame failed (Err says why, never an
// address).
type FrameError struct {
	EventID, VideoID uuid.UUID
	Err              error
}

func (e *FrameError) Error() string {
	return fmt.Sprintf("Event %s video %s: %v", e.EventID, e.VideoID, e.Err)
}

func (e *FrameError) Unwrap() error { return e.Err }

// Pass makes one pass over the videos that need a frame: it claims each in
// order, up to frameBatch of them, and takes its frame (frameJob). A video
// that fails is reported through onError. A pass that finds the frame
// service cannot be asked lets go of its claim and ends with
// ErrFrameServiceDown.
func (w *FrameWorker) Pass(ctx context.Context, onError func(error)) (FrameReport, error) {
	var report FrameReport
	var after frameAfter
	for report.Claimed < frameBatch {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		claim, found, err := w.store.ClaimNextFrame(ctx, w.now().UTC(), after, frameLease)
		if err != nil || !found {
			return report, err
		}
		report.Claimed++
		after = frameAfter{eventID: claim.EventID, videoID: claim.VideoID}
		if down := (frameJob{w: w, claim: claim, counts: &report, onError: onError}).run(ctx); down != nil {
			return report, fmt.Errorf("%w: %v", ErrFrameServiceDown, down)
		}
	}
	return report, nil
}

// frameJob is one claimed video's frame.
type frameJob struct {
	w       *FrameWorker
	claim   FrameClaim
	counts  *FrameReport
	onError func(error)
}

func (j frameJob) report(err error) {
	if j.onError != nil {
		j.onError(&FrameError{EventID: j.claim.EventID, VideoID: j.claim.VideoID, Err: err})
	}
}

// errFrameServiceRefuses is the frame service refusing core's request
// (400): its allowlist does not name the storage, or the two disagree on
// the protocol. No video is at fault.
var errFrameServiceRefuses = errors.New("the frame service refuses core's request; check its MEDIA_FRAME_ALLOWED_HOSTS against core's R2_ENDPOINT")

// run takes the claimed video's frame, and settles the claim however that
// ends. It answers why the frame service cannot be asked (the pass stops
// there), and nil otherwise.
func (j frameJob) run(ctx context.Context) (down error) {
	panicked, err := j.step(ctx)
	var problem *mediaframe.Problem
	switch {
	case panicked:
		j.counts.Panicked++
		j.report(errors.Join(errFramePanicked, j.settle(ctx, func(ctx context.Context) error { return j.w.store.FailFrame(ctx, j.claim) })))
	case err == nil:
	case errors.Is(err, mediaframe.ErrUnavailable), errors.As(err, &problem) && problem.Status == http.StatusBadRequest:
		if problem != nil && problem.Status == http.StatusBadRequest {
			err = fmt.Errorf("%w (%v)", errFrameServiceRefuses, err)
		}
		j.counts.Released++
		if releaseErr := j.settle(ctx, func(ctx context.Context) error { return j.w.store.ReleaseFrame(ctx, j.claim) }); releaseErr != nil {
			j.report(releaseErr)
		}
		return err
	case errors.Is(err, ErrDecodeBusy):
		// The images uploads decode come first; this one waits.
		j.counts.Released++
		if releaseErr := j.settle(ctx, func(ctx context.Context) error { return j.w.store.ReleaseFrame(ctx, j.claim) }); releaseErr != nil {
			j.report(releaseErr)
		}
	case errors.Is(err, mediaframe.ErrNoFrame):
		j.report(fmt.Errorf("it gives no frame, so it has no poster: %w", err))
		if failErr := j.settle(ctx, func(ctx context.Context) error { return j.w.store.FailFrame(ctx, j.claim) }); failErr != nil {
			j.report(failErr)
			return nil
		}
		j.counts.Failed++
	default:
		j.report(err)
		giveUp := j.claim.Attempts+1 >= FrameMaxAttempts
		if deferErr := j.settle(ctx, func(ctx context.Context) error {
			return j.w.store.DeferFrame(ctx, j.claim, j.w.now().UTC(), giveUp)
		}); deferErr != nil {
			j.report(deferErr)
			return nil
		}
		if giveUp {
			j.counts.Abandoned++
		} else {
			j.counts.Retried++
		}
	}
	return nil
}

// settle runs a short transaction on the claim, even when the pass's
// context has ended.
func (j frameJob) settle(ctx context.Context, write func(ctx context.Context) error) error {
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), frameDatabaseTimeout)
	defer cancel()
	return write(dbCtx)
}

func (j frameJob) step(ctx context.Context) (panicked bool, err error) {
	defer func() {
		// Only the video is logged (errFramePanicked), never what the
		// panic held.
		if recover() != nil {
			panicked = true
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, frameWork)
	defer cancel()
	return false, j.take(ctx)
}

// take asks for the frame, stores it and links it to the video.
func (j frameJob) take(ctx context.Context) error {
	w := j.w
	signCtx, cancel := context.WithTimeout(ctx, frameStorageTimeout)
	address, err := w.storage.PresignGet(signCtx, j.claim.VideoKey, frameURLTTL)
	cancel()
	if err != nil {
		return fmt.Errorf("presign the video: %w", err)
	}
	askCtx, cancel := context.WithTimeout(ctx, frameRequestTimeout)
	frame, err := w.frames.Frame(askCtx, address, FrameAt)
	cancel()
	if err != nil {
		return err
	}
	stored, err := w.encode(ctx, frame)
	if err != nil {
		return err
	}
	now := w.now().UTC()
	img := stored.image
	key := stored.keyPrefix + uuid.NewString() + stored.keySuffix
	dbCtx, cancel := context.WithTimeout(ctx, frameDatabaseTimeout)
	created, err := w.store.CreateFrame(dbCtx, Media{
		Type: stored.ctype, Size: int64(len(stored.body)), Kind: stored.kind, Key: key,
		Width: img.size.Width, Height: img.size.Height, SizeObjects: sizeObjectsOf(img.sizes),
		Purpose: w.purpose.Name, Status: StatusPending, Visibility: VisibilityPublic,
		ExpiresAt: pendingExpiry(w.purpose, now), CoverColors: img.coverColors, CoverColorsComputed: true,
		ServingPolicyApplied: true,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("record the frame: %w", err)
	}
	if err := w.put(ctx, key, stored.body, stored.ctype); err != nil {
		return errors.Join(err, j.discard(ctx, created.ID))
	}
	for _, size := range img.sizes {
		if err := w.put(ctx, sizeObjectKey(key, size.name, size.ctype), size.body, size.ctype); err != nil {
			return errors.Join(err, j.discard(ctx, created.ID))
		}
	}
	dbCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), frameDatabaseTimeout)
	attached, err := w.store.AttachFrame(dbCtx, j.claim, created.ID)
	cancel()
	if err != nil {
		return errors.Join(fmt.Errorf("link the frame: %w", err), j.discard(ctx, created.ID))
	}
	if !attached {
		// The video left the Event, or the claim was lost to another
		// worker: this frame is no one's.
		j.counts.Discarded++
		if err := j.discard(ctx, created.ID); err != nil {
			j.report(err)
		}
		return nil
	}
	j.counts.Made++
	return nil
}

// encode runs the frame through the image pipeline of PurposeVideoFrame,
// within the decode budget: re-encoded, with its sizes. A wait the budget
// cannot meet is ErrDecodeBusy.
func (w *FrameWorker) encode(ctx context.Context, frame []byte) (storedFile, error) {
	release, err := w.decoding.Acquire(ctx)
	if err != nil {
		return storedFile{}, err
	}
	defer release()
	stored, err := purposeFile(w.purpose, frame)
	if err != nil {
		return storedFile{}, fmt.Errorf("the frame service answered an image core does not store: %w", err)
	}
	// The purpose takes only JPEG, re-encoded: never an SVG, never bytes
	// kept as they came.
	if stored.image == nil || stored.kind != KindImage || stored.keySuffix != "" {
		return storedFile{}, errors.New("the frame is not a re-encoded raster image")
	}
	return stored, nil
}

func (w *FrameWorker) put(ctx context.Context, key string, data []byte, ctype string) error {
	putCtx, cancel := context.WithTimeout(ctx, frameStorageTimeout)
	defer cancel()
	if err := w.storage.Put(putCtx, key, data, ServingMetadataFor(w.purpose.Name, ctype, "")); err != nil {
		return fmt.Errorf("store the frame: %w", err)
	}
	return nil
}

// discard leaves a frame no video holds to expire at once: the expiry
// cleanup deletes its objects, whichever were written.
func (j frameJob) discard(ctx context.Context, id uuid.UUID) error {
	now := j.w.now().UTC()
	return j.settle(ctx, func(ctx context.Context) error { return j.w.store.ExpireUnattachedAt(ctx, id, &now) })
}

// Run makes passes in the background until ctx ends: at once and every
// framePollInterval; right after a pass that claimed a full batch. While the frame service cannot be asked it waits
// frameServiceDownFirst, doubling up to frameServiceDownMax, and says so
// once. It logs what a pass changed and each video that failed (by id;
// never an address); a pass with nothing to do says nothing. A pass that
// panics outside a video's step is logged, and the next one comes as
// usual.
func (w *FrameWorker) Run(ctx context.Context, logf func(format string, args ...any)) {
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		var downWait time.Duration
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			var next time.Duration
			next, downWait = w.runPass(ctx, logf, downWait)
			timer.Reset(next)
		}
	}()
}

// runPass makes one pass for Run and says when the next one comes.
func (w *FrameWorker) runPass(ctx context.Context, logf func(format string, args ...any), downWait time.Duration) (next, nextDownWait time.Duration) {
	next, nextDownWait = framePollInterval, downWait
	defer func() {
		if recover() != nil {
			logf("media frames: a pass panicked; the next one comes as usual")
		}
	}()
	report, err := w.Pass(ctx, func(err error) { logf("media frames: %v", err) })
	switch {
	case errors.Is(err, ErrFrameServiceDown):
		if downWait == 0 {
			logf("media frames: %v; videos wait for their frames until it answers", err)
		}
		nextDownWait = min(max(2*downWait, frameServiceDownFirst), frameServiceDownMax)
		next = nextDownWait
	case err != nil && ctx.Err() == nil:
		logf("media frames: %v", err)
	}
	if downWait > 0 && err == nil {
		logf("media frames: the frame service answers again")
		nextDownWait = 0
	}
	if report.any() {
		logf("media frames: %d made, %d give no frame, %d given up and %d panicked on (no poster), %d failed (tried again later), %d discarded",
			report.Made, report.Failed, report.Abandoned, report.Panicked, report.Retried, report.Discarded)
	}
	if report.Claimed == frameBatch && err == nil {
		next = 0
	}
	return next, nextDownWait
}
