package mapregion

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/mapimage"
	"github.com/skylab-kulubu/core-backend/internal/mapimage/maptest"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// buildServer is a Protomaps "builds" list and one build, the archive given,
// read by Range.
func buildServer(t *testing.T, archive []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/builds.json":
			_, _ = w.Write([]byte(`[{"key":"20261122.pmtiles","version":"4.15.2"}]`))
		case "/20261122.pmtiles":
			http.ServeContent(w, r, "20261122.pmtiles", time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC), bytes.NewReader(archive))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newStore(t *testing.T) *Store {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return NewPostgresStore(pool)
}

// A requested region is extracted from the latest build into the bucket
// under a new key, ready with its size; the Events in its box whose map was
// given up are drawn again. An extract over the limit fails before any tile
// data is read; a build of another basemap version fails too; a failed
// region is asked for again.
func TestPostgresWorkerExtractsARegion(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	archive := maptest.City(davutLat, davutLng, 2, 28.8, 40.9, 29.1, 41.2, maptest.Metadata)
	srv := buildServer(t, archive)
	uploader := newMemoryUploader()
	w := &Worker{Store: store, Uploader: uploader, MaxBytes: 1 << 30,
		Builds: Builds{ListURL: srv.URL + "/builds.json", Base: srv.URL + "/", Client: srv.Client()}}
	box := mapimage.BBox{MinLon: 28.85, MinLat: 41.0, MaxLon: 28.93, MaxLat: 41.05}
	r, err := store.Create(ctx, "Davutpaşa", box, 15)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "Aynı kutu", box, 15); !errors.Is(err, ErrExists) {
		t.Fatalf("the same box twice: %v", err)
	}
	failed, outside := uuid.New(), uuid.New()
	for id, point := range map[uuid.UUID][2]float64{failed: {davutLat, davutLng}, outside: {39.9, 32.85}} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, location_lat, location_lng, map_state, map_attempts)
			VALUES ($1, 'Etkinlik', 'Yer', 'WEBLAB', $2, $3, 'failed', 1)`, id, point[0], point[1]); err != nil {
			t.Fatal(err)
		}
	}
	if took, err := w.Pass(ctx); !took || err != nil {
		t.Fatalf("pass %v %v", took, err)
	}
	got, err := store.Get(ctx, r.ID)
	if err != nil || got.Status != StatusReady || got.SourceBuild != "20261122" || got.SizeBytes == nil || got.ProgressBytes != *got.SizeBytes ||
		got.ObjectKey != ObjectKeyFor(r, "20261122") || got.ReadyAt == nil {
		t.Fatalf("region %+v %v", got, err)
	}
	object := uploader.objects[got.ObjectKey]
	if int64(len(object)) != *got.SizeBytes {
		t.Fatalf("object of %d bytes, recorded %d", len(object), *got.SizeBytes)
	}
	report, err := mapimage.Check(ctx, memory{b: object}, davutLat, davutLng)
	if err != nil || len(report.Problems) != 0 || report.Version != "4.15.2" {
		t.Fatalf("the extract: %+v %v", report, err)
	}
	var stateIn, stateOut *string
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT map_state FROM events WHERE id = $1), (SELECT map_state FROM events WHERE id = $2)`,
		failed, outside).Scan(&stateIn, &stateOut); err != nil || stateIn != nil || stateOut == nil {
		t.Fatalf("maps given up: in the box %v, outside %v, %v", stateIn, stateOut, err)
	}
	if ready, err := store.ListReady(ctx); err != nil || len(ready) != 1 {
		t.Fatalf("ready %v %v", ready, err)
	}
	if took, err := w.Pass(ctx); took || err != nil {
		t.Fatalf("a pass with nothing to do: %v %v", took, err)
	}

	small := &Worker{Store: store, Uploader: uploader, MaxBytes: 100,
		Builds: Builds{ListURL: srv.URL + "/builds.json", Base: srv.URL + "/", Client: srv.Client()}}
	big, err := store.Create(ctx, "Büyük", mapimage.BBox{MinLon: 28.8, MinLat: 40.9, MaxLon: 29.1, MaxLat: 41.2}, 15)
	if err != nil {
		t.Fatal(err)
	}
	if took, err := small.Pass(ctx); !took || err == nil {
		t.Fatalf("over the limit: %v %v", took, err)
	}
	if got, _ := store.Get(ctx, big.ID); got.Status != StatusFailed || got.ErrorCode != CodeTooLarge {
		t.Fatalf("over the limit: %+v", got)
	}
	if again, err := store.Retry(ctx, big.ID); err != nil || again.Status != StatusRequested || again.ErrorCode != "" {
		t.Fatalf("retry %+v %v", again, err)
	}
	if _, err := store.Retry(ctx, big.ID); !errors.Is(err, ErrState) {
		t.Fatalf("retry of a requested region: %v", err)
	}
	if retired, err := store.Retire(ctx, big.ID); err != nil || retired.Status != StatusRetired {
		t.Fatalf("retire %+v %v", retired, err)
	}

	other := buildServer(t, maptest.City(davutLat, davutLng, 1, 28.8, 40.9, 29.1, 41.2, `{"name":"Protomaps Basemap","version":"5.0.0"}`))
	wrong := &Worker{Store: store, Uploader: uploader, MaxBytes: 1 << 30,
		Builds: Builds{ListURL: other.URL + "/builds.json", Base: other.URL + "/", Client: other.Client()}}
	mismatch, err := store.Create(ctx, "Başka sürüm", mapimage.BBox{MinLon: 28.86, MinLat: 41.0, MaxLon: 28.92, MaxLat: 41.04}, 15)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Pass(ctx); err == nil {
		t.Fatal("a build of version 5 was extracted")
	}
	if got, _ := store.Get(ctx, mismatch.ID); got.Status != StatusFailed || got.ErrorCode != CodeSchemaMismatch {
		t.Fatalf("version 5: %+v", got)
	}
}

// The API's rules: a manager (media:manage) asks for, retries and retires
// regions and sees every one with its state; anyone else sees the ready
// ones only, without their errors, and may change nothing.
func TestPostgresServiceRules(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	svc := &Service{Store: store, Authz: authz.NewAuthorizer(authz.DefaultPolicy()), Address: func(key string) string { return "https://cdn.example.test/" + key }}
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	r, err := svc.Request(ctx, yk, "  İstanbul  ", istanbul, 15)
	if err != nil || r.Name != "İstanbul" || r.Status != StatusRequested || r.BBox != [4]float64{27.95, 40.80, 29.95, 41.60} {
		t.Fatalf("request %+v %v", r, err)
	}
	if _, err := svc.Request(ctx, yk, "Tekrar", istanbul, 15); !errors.Is(err, ErrExists) {
		t.Fatalf("again: %v", err)
	}
	if _, err := svc.Request(ctx, yk, "", istanbul, 15); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no name: %v", err)
	}
	if _, err := svc.Request(ctx, leader, "Ekip", turkiye, 15); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a leader: %v", err)
	}
	if _, err := svc.Retire(ctx, leader, r.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a leader retires: %v", err)
	}
	if listed, err := svc.List(ctx, leader); err != nil || len(listed) != 0 {
		t.Fatalf("a leader sees %v %v", listed, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE map_regions SET status = 'ready', object_key = 'maps/istanbul.pmtiles', size_bytes = 10, ready_at = now() WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	listed, err := svc.List(ctx, leader)
	if err != nil || len(listed) != 1 || listed[0].URL != "https://cdn.example.test/maps/istanbul.pmtiles" || listed[0].SizeBytes != nil {
		t.Fatalf("a leader sees %+v %v", listed, err)
	}
	if _, err := svc.Retry(ctx, yk, r.ID); !errors.Is(err, ErrState) {
		t.Fatalf("retry of a ready region: %v", err)
	}
	if _, err := svc.Retire(ctx, yk, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown region: %v", err)
	}
}
