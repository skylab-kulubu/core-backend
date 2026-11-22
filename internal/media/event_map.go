package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Event maps (docs/event-map.md): when an Event's place gets a point, core
// draws the map around it once (MapSource: the club's own OpenStreetMap
// tiles, drawn by Gotenberg's Chromium) and keeps it as an event_map Media
// in the Event's own column (events.map_media_id). The ticket PDF and the
// ticket page show it. A new point clears it, and the worker draws another.

// ErrNoEventMap is a point the map source has no map of (outside its tiles):
// the Event shows its place without a map, and the point is not tried again.
var ErrNoEventMap = errors.New("media: no map of this point")

// ErrMapSourceDown is a map source that cannot draw now (its tiles or its
// renderer do not answer): the pass stops, no try is counted, and Run waits
// before the next.
var ErrMapSourceDown = errors.New("media: the map source cannot draw now")

// MapSource draws the map of a point, as a JPEG.
type MapSource interface {
	Map(ctx context.Context, lat, lng float64) ([]byte, error)
}

// MapStorage is the public bucket as the map worker writes it.
type MapStorage interface {
	Put(ctx context.Context, key string, data []byte, meta BlobMetadata) error
}

// MapMaxAttempts is how many failed tries a point gets (about a week of
// trying) before it is given up: the Event then shows its place without a
// map until its point changes.
const MapMaxAttempts = 8

const (
	mapPollInterval    = time.Minute
	mapBatch           = 10
	mapRetryFirst      = 10 * time.Minute
	mapRetryMax        = 24 * time.Hour
	mapRequestTimeout  = 90 * time.Second
	mapWork            = mapRequestTimeout + time.Minute
	mapLease           = mapWork + 2*time.Minute
	mapStorageTimeout  = 30 * time.Second
	mapDatabaseTimeout = 10 * time.Second
	mapDownFirst       = 30 * time.Second
	mapDownMax         = 15 * time.Minute
)

var errMapPanicked = errors.New("the map step panicked; the Event gets no map")

// MapWorkerConfig is what the map worker needs.
type MapWorkerConfig struct {
	Store   *PostgresStore
	Storage MapStorage
	Maps    MapSource
	// Catalogue is the reviewed catalogue when left out; it must hold
	// PurposeEventMap.
	Catalogue    Catalogue
	DecodeBudget *DecodeBudget
	Now          func() time.Time
}

// MapWorker draws the map of every Event whose point has none, as the frame
// worker takes videos' frames: each Event is claimed in a short transaction
// before its map is drawn (two replicas never draw one twice); its record is
// written first (pending, expiring), then its object, then the link, under
// the Event's lock and only while the Event still has the point drawn. A
// failure is tried again later, each in a row doubling the wait, and given
// up after MapMaxAttempts; a source that has no map of the point gives up
// at once; one that cannot draw now costs no try.
type MapWorker struct {
	store    *PostgresStore
	storage  MapStorage
	maps     MapSource
	purpose  Purpose
	decoding *DecodeBudget
	now      func() time.Time
	counts   mapCounters
}

// mapCounters are the worker's outcomes since start, for /v1/metrics.
type mapCounters struct {
	made, noMap, abandoned, panicked, retried, discarded, sourceDown atomic.Int64
}

// Prometheus is the worker's counters (skylab_event_maps_total, by
// outcome): maps made; points with no map, given up after their tries, or
// whose step panicked; failures tried again later (a map Gotenberg refused
// among them); maps discarded for a moved point; and passes stopped while
// the source could not draw.
func (w *MapWorker) Prometheus() string {
	if w == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("# HELP skylab_event_maps_total Event maps by outcome (docs/event-map.md).\n# TYPE skylab_event_maps_total counter\n")
	for _, row := range []struct {
		outcome string
		n       int64
	}{
		{"made", w.counts.made.Load()}, {"no_map", w.counts.noMap.Load()}, {"given_up", w.counts.abandoned.Load()},
		{"panicked", w.counts.panicked.Load()}, {"retried", w.counts.retried.Load()}, {"discarded", w.counts.discarded.Load()},
		{"source_down", w.counts.sourceDown.Load()},
	} {
		fmt.Fprintf(&b, "skylab_event_maps_total{outcome=%q} %d\n", row.outcome, row.n)
	}
	return b.String()
}

func (w *MapWorker) count(r MapReport, down bool) {
	w.counts.made.Add(int64(r.Made))
	w.counts.noMap.Add(int64(r.NoMap))
	w.counts.abandoned.Add(int64(r.Abandoned))
	w.counts.panicked.Add(int64(r.Panicked))
	w.counts.retried.Add(int64(r.Retried))
	w.counts.discarded.Add(int64(r.Discarded))
	if down {
		w.counts.sourceDown.Add(1)
	}
}

// NewMapWorker makes the map worker.
func NewMapWorker(config MapWorkerConfig) (*MapWorker, error) {
	if config.Store == nil || config.Storage == nil || config.Maps == nil {
		return nil, errors.New("event maps: a store, storage and a map source are needed")
	}
	catalogue := config.Catalogue
	if catalogue.purposes == nil {
		catalogue = reviewedCatalogue()
	}
	purpose, ok := catalogue.Lookup(PurposeEventMap)
	if !ok {
		return nil, fmt.Errorf("event maps: the catalogue has no %s purpose", PurposeEventMap)
	}
	w := &MapWorker{store: config.Store, storage: config.Storage, maps: config.Maps, purpose: purpose, decoding: config.DecodeBudget, now: config.Now}
	if w.decoding == nil {
		w.decoding = NewDecodeBudget(DecodeBudgetConfig{})
	}
	if w.now == nil {
		w.now = time.Now
	}
	return w, nil
}

// MapReport counts one pass.
type MapReport struct {
	Made, NoMap, Abandoned, Panicked, Retried, Released, Discarded, Claimed int
}

func (r MapReport) any() bool {
	return r.Made+r.NoMap+r.Abandoned+r.Panicked+r.Retried+r.Discarded > 0
}

// MapError is an Event whose map failed (Err says why; never an address
// or a point).
type MapError struct {
	EventID uuid.UUID
	Err     error
}

func (e *MapError) Error() string { return fmt.Sprintf("Event %s: %v", e.EventID, e.Err) }

func (e *MapError) Unwrap() error { return e.Err }

// Pass draws the maps that are due, up to mapBatch of them, in Event id
// order. It ends with ErrMapSourceDown when the source cannot draw.
func (w *MapWorker) Pass(ctx context.Context, onError func(error)) (MapReport, error) {
	var report MapReport
	var after uuid.UUID
	for report.Claimed < mapBatch {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		claim, found, err := w.store.ClaimNextMap(ctx, w.now().UTC(), after, mapLease)
		if err != nil || !found {
			return report, err
		}
		report.Claimed++
		after = claim.EventID
		if down := (mapJob{w: w, claim: claim, counts: &report, onError: onError}).run(ctx); down != nil {
			return report, fmt.Errorf("%w: %v", ErrMapSourceDown, down)
		}
	}
	return report, nil
}

type mapJob struct {
	w       *MapWorker
	claim   MapClaim
	counts  *MapReport
	onError func(error)
}

func (j mapJob) report(err error) {
	if j.onError != nil {
		j.onError(&MapError{EventID: j.claim.EventID, Err: err})
	}
}

func (j mapJob) settle(ctx context.Context, write func(ctx context.Context) error) error {
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mapDatabaseTimeout)
	defer cancel()
	return write(dbCtx)
}

func (j mapJob) release(ctx context.Context) {
	j.counts.Released++
	if err := j.settle(ctx, func(ctx context.Context) error { return j.w.store.ReleaseMap(ctx, j.claim) }); err != nil {
		j.report(err)
	}
}

// run draws the claimed Event's map and settles the claim however that
// ends; it answers why the source cannot draw (the pass stops there).
func (j mapJob) run(ctx context.Context) (down error) {
	panicked, err := j.step(ctx)
	switch {
	case panicked:
		j.counts.Panicked++
		j.report(errors.Join(errMapPanicked, j.settle(ctx, func(ctx context.Context) error { return j.w.store.FailMap(ctx, j.claim) })))
	case err == nil:
	case ctx.Err() != nil:
		j.release(ctx)
	case errors.Is(err, ErrMapSourceDown):
		j.release(ctx)
		return err
	case errors.Is(err, ErrDecodeBusy):
		j.release(ctx)
	case errors.Is(err, ErrNoEventMap):
		if failErr := j.settle(ctx, func(ctx context.Context) error { return j.w.store.FailMap(ctx, j.claim) }); failErr != nil {
			j.report(failErr)
			return nil
		}
		j.counts.NoMap++
	default:
		j.report(err)
		giveUp := j.claim.Attempts+1 >= MapMaxAttempts
		if deferErr := j.settle(ctx, func(ctx context.Context) error {
			return j.w.store.DeferMap(ctx, j.claim, j.w.now().UTC(), giveUp)
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

func (j mapJob) step(ctx context.Context) (panicked bool, err error) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, mapWork)
	defer cancel()
	return false, j.draw(ctx)
}

// draw asks for the map, stores it and links it to the Event.
func (j mapJob) draw(ctx context.Context) error {
	w := j.w
	askCtx, cancel := context.WithTimeout(ctx, mapRequestTimeout)
	picture, err := w.maps.Map(askCtx, j.claim.Lat, j.claim.Lng)
	cancel()
	if err != nil {
		return err
	}
	release, err := w.decoding.Acquire(ctx)
	if err != nil {
		return err
	}
	stored, err := purposeFile(w.purpose, picture)
	release()
	if err != nil {
		return fmt.Errorf("the map source answered an image core does not store: %w", err)
	}
	if stored.image == nil || stored.kind != KindImage || stored.keySuffix != "" {
		return errors.New("the map is not a re-encoded raster image")
	}
	now := w.now().UTC()
	img := stored.image
	key := stored.keyPrefix + uuid.NewString() + stored.keySuffix
	dbCtx, cancel := context.WithTimeout(ctx, mapDatabaseTimeout)
	created, err := w.store.CreateMapRecord(dbCtx, Media{
		Type: stored.ctype, Size: int64(len(stored.body)), Kind: stored.kind, Key: key,
		Width: img.size.Width, Height: img.size.Height, SizeObjects: sizeObjectsOf(img.sizes),
		Purpose: w.purpose.Name, Status: StatusPending, Visibility: VisibilityPublic,
		ExpiresAt: pendingExpiry(w.purpose, now), CoverColors: img.coverColors, CoverColorsComputed: true,
		ServingPolicyApplied: true,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("record the map: %w", err)
	}
	if err := w.put(ctx, key, stored.body, stored.ctype); err != nil {
		return errors.Join(err, j.discard(ctx, created.ID))
	}
	for _, size := range img.sizes {
		if err := w.put(ctx, sizeObjectKey(key, size.name, size.ctype), size.body, size.ctype); err != nil {
			return errors.Join(err, j.discard(ctx, created.ID))
		}
	}
	dbCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), mapDatabaseTimeout)
	attached, err := w.store.AttachMap(dbCtx, j.claim, created.ID)
	cancel()
	if err != nil {
		return errors.Join(fmt.Errorf("link the map: %w", err), j.discard(ctx, created.ID))
	}
	if !attached {
		// The point moved, or the Event went, meanwhile: this map is no
		// one's. The claim is let go (a writer that moved the point without
		// clearing it, the release before this one, leaves it), so the new
		// point is drawn next.
		j.counts.Discarded++
		if err := errors.Join(j.discard(ctx, created.ID),
			j.settle(ctx, func(ctx context.Context) error { return w.store.ReleaseMap(ctx, j.claim) })); err != nil {
			j.report(err)
		}
		return nil
	}
	j.counts.Made++
	return nil
}

func (w *MapWorker) put(ctx context.Context, key string, data []byte, ctype string) error {
	putCtx, cancel := context.WithTimeout(ctx, mapStorageTimeout)
	defer cancel()
	if err := w.storage.Put(putCtx, key, data, ServingMetadataFor(w.purpose.Name, ctype, "")); err != nil {
		return fmt.Errorf("store the map: %w", err)
	}
	return nil
}

func (j mapJob) discard(ctx context.Context, id uuid.UUID) error {
	now := j.w.now().UTC()
	return j.settle(ctx, func(ctx context.Context) error { return j.w.store.ExpireUnattachedAt(ctx, id, &now) })
}

// Run makes passes in the background until ctx ends, at once and every
// mapPollInterval, right after a full one; while the source cannot draw it
// waits mapDownFirst, doubling up to mapDownMax, and says so once. It logs
// what a pass changed and each Event that failed, by id. The channel closes
// once the pass in flight has settled its claims after ctx ends.
func (w *MapWorker) Run(ctx context.Context, logf func(format string, args ...any)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	return done
}

func (w *MapWorker) runPass(ctx context.Context, logf func(format string, args ...any), downWait time.Duration) (next, nextDownWait time.Duration) {
	next, nextDownWait = mapPollInterval, downWait
	defer func() {
		if recover() != nil {
			logf("event maps: a pass panicked; the next one comes as usual")
		}
	}()
	report, err := w.Pass(ctx, func(err error) { logf("event maps: %v", err) })
	w.count(report, errors.Is(err, ErrMapSourceDown))
	switch {
	case errors.Is(err, ErrMapSourceDown):
		if downWait == 0 {
			logf("event maps: %v; maps wait until it draws again", err)
		}
		nextDownWait = min(max(2*downWait, mapDownFirst), mapDownMax)
		next = nextDownWait
	case err != nil && ctx.Err() == nil:
		logf("event maps: %v", err)
	}
	if downWait > 0 && err == nil {
		logf("event maps: the map source draws again")
		nextDownWait = 0
	}
	if report.any() {
		logf("event maps: %d made, %d without a map of their point, %d given up and %d panicked on, %d failed (tried again later), %d discarded",
			report.Made, report.NoMap, report.Abandoned, report.Panicked, report.Retried, report.Discarded)
	}
	if report.Claimed == mapBatch && err == nil {
		next = 0
	}
	return next, nextDownWait
}
