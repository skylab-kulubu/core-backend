package media_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/media/s3test"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// Event maps (docs/event-map.md): the worker draws the map of every Event
// whose point has none, once.

type mapDatabase struct {
	pool  *pgxpool.Pool
	store *media.PostgresStore
	r2    *media.R2
	fake  *s3test.Server
	now   time.Time
}

func newMapDatabase(t *testing.T) *mapDatabase {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	r2, fake := multipartR2(t)
	return &mapDatabase{pool: pool, store: media.NewPostgresStore(pool), r2: r2, fake: fake, now: time.Now().UTC()}
}

// fakeMaps answers a 1280×480 JPEG, or what answer says, and records the
// points it was asked for.
type fakeMaps struct {
	mu     sync.Mutex
	asked  [][2]float64
	answer func(lat, lng float64) ([]byte, error)
}

func (f *fakeMaps) Map(_ context.Context, lat, lng float64) ([]byte, error) {
	f.mu.Lock()
	f.asked = append(f.asked, [2]float64{lat, lng})
	answer := f.answer
	f.mu.Unlock()
	if answer != nil {
		return answer(lat, lng)
	}
	return nil, errors.New("no answer")
}

func (f *fakeMaps) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// event is an Event with a point whose wait is over.
func (d *mapDatabase) event(t *testing.T, lat, lng float64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := d.pool.Exec(context.Background(), `INSERT INTO events (id, name, location, owner_team, location_lat, location_lng)
		VALUES ($1, 'Gecekodu', 'YTÜ Davutpaşa', 'WEBLAB', $2, $3)`, id, lat, lng); err != nil {
		t.Fatal(err)
	}
	return id
}

func (d *mapDatabase) worker(t *testing.T, maps media.MapSource) *media.MapWorker {
	t.Helper()
	w, err := media.NewMapWorker(media.MapWorkerConfig{Store: d.store, Storage: d.r2, Maps: maps, Now: func() time.Time { return d.now }})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func mapPass(t *testing.T, w *media.MapWorker) media.MapReport {
	t.Helper()
	report, err := w.Pass(context.Background(), func(err error) { t.Logf("map: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return report
}

type eventMap struct {
	media    *uuid.UUID
	state    string
	attempts int
	retryAt  *time.Time
	claimed  bool
}

func (d *mapDatabase) mapOf(t *testing.T, id uuid.UUID) eventMap {
	t.Helper()
	var m eventMap
	if err := d.pool.QueryRow(context.Background(), `SELECT map_media_id, COALESCE(map_state, ''), map_attempts, map_retry_at, map_claim_id IS NOT NULL
		FROM events WHERE id = $1`, id).Scan(&m.media, &m.state, &m.attempts, &m.retryAt, &m.claimed); err != nil {
		t.Fatal(err)
	}
	return m
}

func (d *mapDatabase) attachedAsMap(t *testing.T, mediaID, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(), `SELECT count(*) FROM media_attachments
		WHERE media_id = $1 AND owner_service = 'core' AND owner_type = 'event' AND owner_id = $2 AND role = 'event_map'`,
		mediaID, eventID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An Event with a point gets its map: the source is asked for the point,
// its JPEG goes through the image pipeline and is stored as a public
// event_map Media with no uploader, which the Event's column holds and
// attaches in the role event_map. An Event with its map is done; one
// without a point is never asked for.
func TestPostgresMapWorkerDrawsAndAttachesAnEventsMap(t *testing.T) {
	d := newMapDatabase(t)
	id := d.event(t, 41.025376, 28.889648)
	if _, err := d.pool.Exec(context.Background(), `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Online', 'Online', 'WEBLAB')`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	maps := &fakeMaps{answer: func(float64, float64) ([]byte, error) { return testFrameJPEG(t, 1280, 480), nil }}
	w := d.worker(t, maps)
	if report := mapPass(t, w); report.Made != 1 || report.Claimed != 1 {
		t.Fatalf("pass %+v", report)
	}
	if len(maps.asked) != 1 || maps.asked[0] != [2]float64{41.025376, 28.889648} {
		t.Fatalf("asked %v", maps.asked)
	}
	m := d.mapOf(t, id)
	if m.media == nil || m.state != "" || m.attempts != 0 || m.claimed || m.retryAt != nil {
		t.Fatalf("map %+v", m)
	}
	got, err := d.store.GetIncludingDeleted(context.Background(), *m.media)
	if err != nil {
		t.Fatal(err)
	}
	if got.Purpose != media.PurposeEventMap || got.Type != "image/jpeg" || got.Visibility != media.VisibilityPublic ||
		got.Status != media.StatusAttached || got.UploadedBy != uuid.Nil || got.Width != 1280 || got.Height != 480 || !strings.HasPrefix(got.Key, "images/") {
		t.Fatalf("map Media %+v", got)
	}
	if object, ok := d.fake.Object("media", got.Key); !ok || object.ContentType != "image/jpeg" {
		t.Fatalf("object %v %+v", ok, object)
	}
	if n := d.attachedAsMap(t, got.ID, id); n != 1 {
		t.Fatalf("attached %d times", n)
	}
	if report := mapPass(t, w); report.Claimed != 0 || maps.calls() != 1 {
		t.Fatalf("drawn again: %+v", report)
	}
}

// A point just set waits a minute (a pin moved a few times is drawn once);
// moving it clears the map, whose Media is detached, and the worker draws
// the new one. A copy with the same point shares the map.
func TestPostgresANewPointGetsANewMap(t *testing.T) {
	d := newMapDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(d.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	lat, lng := 41.025376, 28.889648
	created, err := events.Create(ctx, yk, event.Event{Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB", LocationLat: &lat, LocationLng: &lng,
		StartDate: func() *time.Time { v := d.now.Add(30 * 24 * time.Hour); return &v }()})
	if err != nil {
		t.Fatal(err)
	}
	maps := &fakeMaps{answer: func(float64, float64) ([]byte, error) { return testFrameJPEG(t, 1280, 480), nil }}
	w := d.worker(t, maps)
	if report := mapPass(t, w); report.Claimed != 0 {
		t.Fatalf("a point just set was drawn: %+v", report)
	}
	d.now = d.now.Add(2 * time.Minute)
	if report := mapPass(t, w); report.Made != 1 {
		t.Fatalf("pass %+v", report)
	}
	first := d.mapOf(t, created.ID).media
	read, err := events.Get(ctx, &yk, created.ID)
	if err != nil || first == nil || read.MapImageURL == "" || read.MapAttribution != event.MapAttribution || read.MapAttributionURL != event.MapAttributionURL || read.MapImageID == nil || *read.MapImageID != *first {
		t.Fatalf("Event's map %q %q %v %v", read.MapImageURL, read.MapAttribution, read.MapImageID, err)
	}

	copied, _, err := events.Copy(ctx, yk, created.ID, event.CopyInput{ID: uuid.New(), Name: "Gecekodu 2", StartDate: func() *time.Time { v := d.now.Add(400 * 24 * time.Hour); return &v }()})
	if err != nil {
		t.Fatal(err)
	}
	if shared := d.mapOf(t, copied.Event.ID).media; shared == nil || *shared != *first || d.attachedAsMap(t, *first, copied.Event.ID) != 1 {
		t.Fatalf("the copy's map %v", shared)
	}

	// A body without the point keeps the map.
	if _, err := events.Patch(ctx, yk, created.ID, patchBody(t, `{"capacity":40}`)); err != nil {
		t.Fatal(err)
	}
	if kept := d.mapOf(t, created.ID).media; kept == nil || *kept != *first {
		t.Fatalf("the map went without a new point: %v", kept)
	}
	if _, err := events.Patch(ctx, yk, created.ID, patchBody(t, `{"locationLat":41.043123,"locationLng":29.009}`)); err != nil {
		t.Fatal(err)
	}
	moved := d.mapOf(t, created.ID)
	if moved.media != nil || moved.retryAt == nil || d.attachedAsMap(t, *first, created.ID) != 0 {
		t.Fatalf("after the move %+v", moved)
	}
	d.now = d.now.Add(2 * time.Minute)
	if report := mapPass(t, w); report.Made != 1 {
		t.Fatalf("pass %+v", report)
	}
	if last := maps.asked[len(maps.asked)-1]; last != [2]float64{41.043123, 29.009} {
		t.Fatalf("asked %v", maps.asked)
	}
	// The copy keeps the old map, still attached.
	if shared := d.mapOf(t, copied.Event.ID).media; shared == nil || *shared != *first {
		t.Fatalf("the copy lost its map: %v", shared)
	}
}

// A failure is tried again later, each in a row twice as late, and given up
// after MapMaxAttempts; a point the source has no map of is given up at
// once; a source that cannot draw costs no try and stops the pass.
func TestPostgresMapWorkerBacksOffGivesUpAndWaits(t *testing.T) {
	d := newMapDatabase(t)
	failing := d.event(t, 41.0, 29.0)
	maps := &fakeMaps{answer: func(float64, float64) ([]byte, error) { return nil, errors.New("tile broken") }}
	w := d.worker(t, maps)
	if report := mapPass(t, w); report.Retried != 1 {
		t.Fatalf("pass %+v", report)
	}
	m := d.mapOf(t, failing)
	if m.attempts != 1 || m.retryAt == nil || !m.retryAt.Equal(d.now.Add(10*time.Minute).Truncate(time.Microsecond)) || m.claimed {
		t.Fatalf("after one failure %+v (now %s)", m, d.now)
	}
	for i := 1; i < media.MapMaxAttempts; i++ {
		d.now = d.now.Add(48 * time.Hour)
		mapPass(t, w)
	}
	if m := d.mapOf(t, failing); m.state != "failed" || m.attempts != media.MapMaxAttempts {
		t.Fatalf("after %d failures %+v", media.MapMaxAttempts, m)
	}

	outside := d.event(t, 60.0, 10.0)
	maps.mu.Lock()
	maps.answer = func(float64, float64) ([]byte, error) { return nil, media.ErrNoEventMap }
	maps.mu.Unlock()
	if report := mapPass(t, w); report.NoMap != 1 {
		t.Fatalf("pass %+v", report)
	}
	if m := d.mapOf(t, outside); m.state != "failed" || m.attempts != 1 {
		t.Fatalf("no map of the point %+v", m)
	}

	down := d.event(t, 41.1, 29.1)
	maps.mu.Lock()
	maps.answer = func(float64, float64) ([]byte, error) { return nil, media.ErrMapSourceDown }
	maps.mu.Unlock()
	if _, err := w.Pass(context.Background(), nil); !errors.Is(err, media.ErrMapSourceDown) {
		t.Fatalf("pass %v", err)
	}
	if m := d.mapOf(t, down); m.attempts != 0 || m.claimed || m.state != "" {
		t.Fatalf("while the source is down %+v", m)
	}
}

// A point moved while its map was being drawn: the map is no one's and
// expires at once; the new point is drawn next.
func TestPostgresAMapOfAMovedPointIsDiscarded(t *testing.T) {
	d := newMapDatabase(t)
	id := d.event(t, 41.0, 29.0)
	maps := &fakeMaps{answer: func(float64, float64) ([]byte, error) {
		if _, err := d.pool.Exec(context.Background(), `UPDATE events SET location_lat = 41.5, location_lng = 29.5 WHERE id = $1`, id); err != nil {
			t.Error(err)
		}
		return testFrameJPEG(t, 1280, 480), nil
	}}
	if report := mapPass(t, d.worker(t, maps)); report.Discarded != 1 || report.Made != 0 {
		t.Fatalf("pass %+v", report)
	}
	m := d.mapOf(t, id)
	if m.media != nil || m.claimed {
		t.Fatalf("map %+v", m)
	}
	var expired int
	if err := d.pool.QueryRow(context.Background(), `SELECT count(*) FROM media WHERE purpose = 'event_map' AND expires_at <= now()`).Scan(&expired); err != nil || expired != 1 {
		t.Fatalf("discarded maps %d %v", expired, err)
	}
}

func patchBody(t *testing.T, body string) event.Patch {
	t.Helper()
	var p event.Patch
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A map Gotenberg always refuses is that Event's failure, not the source's:
// its try is counted and the pass goes on to the next Event, so one point
// never holds up the others. After MapMaxAttempts it is given up, and the
// counters say so.
func TestPostgresARefusedMapDoesNotHoldUpTheQueue(t *testing.T) {
	d := newMapDatabase(t)
	poisoned, fine := d.event(t, 41.0, 29.0), d.event(t, 41.2, 29.2)
	if poisoned.String() > fine.String() {
		poisoned, fine = fine, poisoned
		if _, err := d.pool.Exec(context.Background(), `UPDATE events SET location_lat = CASE WHEN id = $1 THEN 41.0 ELSE 41.2 END,
			location_lng = CASE WHEN id = $1 THEN 29.0 ELSE 29.2 END WHERE id IN ($1, $2)`, poisoned, fine); err != nil {
			t.Fatal(err)
		}
	}
	maps := &fakeMaps{answer: func(lat, _ float64) ([]byte, error) {
		if lat == 41.0 {
			return nil, errors.New("mapimage: the map's picture was refused: certificate: invalid")
		}
		return testFrameJPEG(t, 1280, 480), nil
	}}
	w := d.worker(t, maps)
	report := mapPass(t, w)
	if report.Retried != 1 || report.Made != 1 || report.Claimed != 2 {
		t.Fatalf("pass %+v", report)
	}
	if m := d.mapOf(t, fine); m.media == nil {
		t.Fatalf("the other Event waited: %+v", m)
	}
	for i := 1; i < media.MapMaxAttempts; i++ {
		d.now = d.now.Add(48 * time.Hour)
		mapPass(t, w)
	}
	if m := d.mapOf(t, poisoned); m.state != "failed" || m.attempts != media.MapMaxAttempts {
		t.Fatalf("the refused map %+v", m)
	}
	if text := w.Prometheus(); !strings.Contains(text, "# TYPE skylab_event_maps_total counter") || !strings.Contains(text, `skylab_event_maps_total{outcome="made"}`) {
		t.Fatalf("metrics %q", text)
	}
}
