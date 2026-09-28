package media_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// hookedScanStorage is the public bucket with hooks after a copy and a
// switch that fails deletes of served keys.
type hookedScanStorage struct {
	*media.MemoryBlob
	mu           sync.Mutex
	afterCopy    func()
	failServedRm bool
}

func (h *hookedScanStorage) Copy(ctx context.Context, from, to string, meta media.BlobMetadata) error {
	err := h.MemoryBlob.Copy(ctx, from, to, meta)
	h.mu.Lock()
	after := h.afterCopy
	h.mu.Unlock()
	if after != nil {
		after()
	}
	return err
}

func (h *hookedScanStorage) Delete(ctx context.Context, key string) error {
	h.mu.Lock()
	fail := h.failServedRm
	h.mu.Unlock()
	if fail && strings.HasPrefix(key, "files/") {
		return errors.New("r2: 503")
	}
	return h.MemoryBlob.Delete(ctx, key)
}

func (d *scanDatabase) workerOn(storage media.ScanStorage, now func() time.Time) *media.ScanWorker {
	w, err := media.NewScanWorker(media.ScanWorkerConfig{
		Store: d.store, Scanner: d.client, Public: storage,
		Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config())), Now: now,
	})
	if err != nil {
		panic(err)
	}
	return w
}

func passOf(t *testing.T, w *media.ScanWorker) media.ScanReport {
	t.Helper()
	report, err := w.Pass(context.Background(), func(err error) { t.Logf("scan: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// Two core replicas run side by side on every rolling deploy. A scan is
// claimed before any clamd or storage work: while one worker scans a held
// club file, the other leaves it alone. It is scanned once, and its served
// copy is there.
func TestPostgresTwoWorkersScanAHeldFileOnce(t *testing.T) {
	d := newScanDatabase(t)
	club := d.heldClubFile(t, "data.pdf", pdfFile())
	aCopied, bDone := make(chan struct{}), make(chan struct{})
	a := d.workerOn(&hookedScanStorage{MemoryBlob: d.blobs, afterCopy: func() { close(aCopied); <-bDone }}, func() time.Time { return d.now })
	b := d.workerOn(d.blobs, func() time.Time { return d.now })

	aReport := make(chan media.ScanReport, 1)
	go func() { aReport <- passOf(t, a) }()
	select {
	case <-aCopied:
	case <-time.After(30 * time.Second):
		t.Fatal("the first worker never copied")
	}
	if report := passOf(t, b); report.Clean+report.Rejected+report.Failed != 0 {
		t.Fatalf("the second worker took a claimed Media: %+v", report)
	}
	close(bDone)
	if report := <-aReport; report.Clean != 1 {
		t.Fatalf("first worker %+v", report)
	}
	if n := len(d.clamd.Streams()); n != 1 {
		t.Fatalf("scanned %d times", n)
	}
	got := d.get(t, club.ID)
	if _, ok := d.blobs.Get(got.Key); got.Status != media.StatusPending || got.Key != "files/"+club.ID.String() || !ok {
		t.Fatalf("%s at %s, object present %v", got.Status, got.Key, ok)
	}
}

// A worker whose lease ran out lost its claim: another worker scanned the
// file meanwhile and serves it at the same key. The first must not delete
// what the Media now points to.
func TestPostgresAWorkerThatLostItsClaimDeletesNothing(t *testing.T) {
	d := newScanDatabase(t)
	club := d.heldClubFile(t, "data.pdf", pdfFile())
	late := func() time.Time { return d.now.Add(24 * time.Hour) }
	var bReport media.ScanReport
	a := d.workerOn(&hookedScanStorage{MemoryBlob: d.blobs, afterCopy: func() {
		// The first worker stalls past its lease; the second claims and
		// finishes the scan.
		bReport = passOf(t, d.workerOn(d.blobs, late))
	}}, func() time.Time { return d.now })
	if report := passOf(t, a); report.Clean != 0 {
		t.Fatalf("the worker that lost its claim counted %+v", report)
	}
	if bReport.Clean != 1 {
		t.Fatalf("second worker %+v", bReport)
	}
	got := d.get(t, club.ID)
	if _, ok := d.blobs.Get(got.Key); got.Status != media.StatusPending || got.Key != "files/"+club.ID.String() || !ok {
		t.Fatalf("%s at %s, object present %v", got.Status, got.Key, ok)
	}
}

// A purge takes a held file's served copy too: a clean copy that landed
// before the purge (the scan then finds its Media gone, and its own delete
// of the copy fails) leaves nothing at files/<id>.
func TestPostgresAPurgeAfterTheCleanCopyLeavesNothingServed(t *testing.T) {
	d := newScanDatabase(t)
	club := d.heldClubFile(t, "data.pdf", pdfFile())
	buckets := media.Buckets{Public: d.blobs, Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config()))}
	storage := &hookedScanStorage{MemoryBlob: d.blobs, failServedRm: true}
	storage.afterCopy = func() {
		report, err := media.PurgeExpired(context.Background(), d.store, buckets, club.ExpiresAt.Add(time.Second), func(err error) { t.Log(err) })
		if err != nil || report.Purged != 1 {
			t.Errorf("purge %+v, err %v", report, err)
		}
	}
	w := d.workerOn(storage, func() time.Time { return d.now })
	passOf(t, w)
	passOf(t, w)
	got := d.get(t, club.ID)
	if got.BlobPurgedAt == nil {
		t.Fatalf("not purged: %s", got.Status)
	}
	if keys := d.blobs.Keys(); len(keys) != 0 {
		t.Fatalf("left in the public bucket: %v", keys)
	}
}

// A scan that crashed after its clean copy landed leaves the Media held
// and waiting: the archive and expiry purges take the copy with it.
func TestPostgresAPurgeTakesTheCopyOfAScanThatCrashed(t *testing.T) {
	ctx := context.Background()
	for name, purge := range map[string]func(d *scanDatabase, club media.Media, buckets media.Buckets) error{
		"expiry": func(d *scanDatabase, club media.Media, buckets media.Buckets) error {
			_, err := media.PurgeExpired(ctx, d.store, buckets, club.ExpiresAt.Add(time.Second), func(err error) { t.Error(err) })
			return err
		},
		"archive": func(d *scanDatabase, club media.Media, buckets media.Buckets) error {
			if err := d.svc.Delete(ctx, d.organizer, club.ID); err != nil {
				return err
			}
			_, err := media.PurgeDeleted(ctx, d.store, buckets, time.Now().Add(31*24*time.Hour), 30*24*time.Hour, 25)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := newScanDatabase(t)
			club := d.heldClubFile(t, "data.pdf", pdfFile())
			if err := d.blobs.Put(ctx, "files/"+club.ID.String(), pdfFile(), media.BlobMetadata{ContentType: "application/pdf"}); err != nil {
				t.Fatal(err)
			}
			if err := purge(d, club, media.Buckets{Public: d.blobs, Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config()))}); err != nil {
				t.Fatal(err)
			}
			if got := d.get(t, club.ID); got.BlobPurgedAt == nil {
				t.Fatalf("not purged: %s", got.Status)
			}
			if keys := d.blobs.Keys(); len(keys) != 0 {
				t.Fatalf("left in the public bucket: %v", keys)
			}
		})
	}
}

// While a worker holds a scan's claim, the expiry purge leaves the Media:
// a copy the scan makes can never land after the purge.
func TestPostgresTheExpiryPurgeWaitsForAScanUnderWay(t *testing.T) {
	d := newScanDatabase(t)
	club := d.heldClubFile(t, "data.pdf", pdfFile())
	buckets := media.Buckets{Public: d.blobs, Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config()))}
	storage := &hookedScanStorage{MemoryBlob: d.blobs}
	storage.afterCopy = func() {
		report, err := media.PurgeExpired(context.Background(), d.store, buckets, d.now.Add(time.Minute), func(err error) { t.Log(err) })
		if err != nil || report.Purged != 0 {
			t.Errorf("purged during the scan: %+v, err %v", report, err)
		}
	}
	// The Media's expiry is due by the purge's clock.
	if _, err := d.pool.Exec(context.Background(), `UPDATE media SET expires_at = $2 WHERE id = $1`, club.ID, d.now); err != nil {
		t.Fatal(err)
	}
	if report := passOf(t, d.workerOn(storage, func() time.Time { return d.now })); report.Clean != 1 {
		t.Fatalf("scan %+v", report)
	}
	got := d.get(t, club.ID)
	if _, ok := d.blobs.Get(got.Key); got.Status != media.StatusPending || !ok {
		t.Fatalf("%s at %s, object present %v", got.Status, got.Key, ok)
	}
}

// midCopyStorage splits a copy into reading its source and writing its
// destination, with a hook between them: an R2 copy that has read the held
// file and lands later.
type midCopyStorage struct {
	*media.MemoryBlob
	mu      sync.Mutex
	midCopy func()
}

func (s *midCopyStorage) Copy(ctx context.Context, from, to string, meta media.BlobMetadata) error {
	data, err := s.MemoryBlob.Read(ctx, from)
	if err != nil {
		return err
	}
	s.mu.Lock()
	hook := s.midCopy
	s.midCopy = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.MemoryBlob.Put(ctx, to, data, meta)
}

// A purge that does not wait for the scan's claim (account erasure), or
// that runs once the claim's lease is over by its clock, can delete a held
// file and its served key while the scan's copy is in flight; the copy then
// lands. A Media whose purge has begun is never marked clean again, so the
// scan deletes the copy it made: nothing is left at files/<id>.
func TestPostgresACopyThatLandsAfterThePurgeIsDeleted(t *testing.T) {
	ctx := context.Background()
	for name, purge := range map[string]func(t *testing.T, d *scanDatabase, held media.Media){
		"erasure": func(t *testing.T, d *scanDatabase, held media.Media) {
			// A personal purpose, so that the erasure purges the held file
			// rather than keep it as club content (no reviewed purpose is a
			// held personal one today).
			if _, err := d.pool.Exec(ctx, `UPDATE media SET purpose = 'profile_picture' WHERE id = $1`, held.ID); err != nil {
				t.Fatal(err)
			}
			users := user.NewPostgresStore(d.pool)
			if _, err := users.RequestDeletion(ctx, d.uploader(), nil); err != nil {
				t.Fatal(err)
			}
			if err := users.AnonymizeAccount(ctx, d.uploader(), time.Now().UTC(), nil); err != nil {
				t.Fatal(err)
			}
			buckets := media.Buckets{Public: d.blobs, Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config()))}
			d.midCopy = func() {
				if err := media.NewImmediateBlobEraser(d.store, buckets).EnsureErased(ctx, held.ID, time.Now().UTC()); err != nil {
					t.Errorf("erase: %v", err)
				}
			}
		},
		"expiry past the lease": func(t *testing.T, d *scanDatabase, held media.Media) {
			buckets := media.Buckets{Public: d.blobs, Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config()))}
			d.midCopy = func() {
				report, err := media.PurgeExpired(ctx, d.store, buckets, held.ExpiresAt.Add(48*time.Hour), func(err error) { t.Log(err) })
				if err != nil || report.Purged != 1 {
					t.Errorf("purge %+v, err %v", report, err)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := newScanDatabase(t)
			held := d.heldClubFile(t, "x.pdf", pdfFile())
			purge(t, d, held)
			storage := &midCopyStorage{MemoryBlob: d.blobs, midCopy: d.midCopy}
			w := d.workerOn(storage, func() time.Time { return d.now })
			passOf(t, w)
			passOf(t, w)
			if got := d.get(t, held.ID); got.BlobPurgedAt == nil {
				t.Fatalf("not purged: %s", got.Status)
			}
			if keys := d.blobs.Keys(); len(keys) != 0 {
				t.Fatalf("left in the public bucket: %v", keys)
			}
		})
	}
}
