package media_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
)

// zipOf is a ZIP of the named members, deflated.
func zipOf(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for name, data := range members {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// rangeWatch is the public bucket that notes, at every ranged read, how
// many database connections the pool has handed out.
type rangeWatch struct {
	*media.MemoryBlob
	d      *scanDatabase
	mu     sync.Mutex
	reads  int
	leased []int32
}

func (r *rangeWatch) OpenRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	r.mu.Lock()
	r.reads++
	r.leased = append(r.leased, r.d.pool.Stat().AcquiredConns())
	r.mu.Unlock()
	return r.MemoryBlob.OpenRange(ctx, key, off, n)
}

// zipWorker is a scan worker on the test's bucket and clock with clamd's
// limits scaled down: 1 MiB a member, 4 MiB in all.
func (d *scanDatabase) zipWorker(storage media.ScanStorage) *media.ScanWorker {
	return media.NewScanWorker(media.ScanWorkerConfig{
		Store: d.store, Scanner: d.client, Public: storage,
		Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config())),
		Now:     func() time.Time { return d.now },
		Limits:  media.ScanLimits{MaxFileSize: 1 << 20, MaxScanSize: 4 << 20, MaxFiles: 100, MaxRecursion: 17},
	})
}

// Before a ZIP reaches clamd, the scan worker reads it by ranges, holding no
// database connection, and rejects one clamd could not scan whole: one that
// holds more than clamd's limits is too_large_to_scan, one malformed or
// holding what clamd cannot read is archive_invalid. Neither reaches clamd;
// each is rejected as any other (its held file deleted, the rejection
// recorded without a signature) and reported with why, by member number.
func TestPostgresScanRejectsAZIPClamdCannotScanWhole(t *testing.T) {
	d := newScanDatabase(t)
	watch := &rangeWatch{MemoryBlob: d.blobs, d: d}
	worker := d.zipWorker(watch)

	tooLarge := d.held(t, "veri.zip", "application/zip", zipOf(t, map[string][]byte{"zeros.bin": make([]byte, 2<<20)}))
	encrypted := zipOf(t, map[string][]byte{"secret.txt": []byte("secret")})
	for _, sig := range [][]byte{[]byte("PK\x03\x04"), []byte("PK\x01\x02")} {
		at := bytes.Index(encrypted, sig)
		flagsAt := at + 6
		if sig[2] == 1 {
			flagsAt = at + 8
		}
		encrypted[flagsAt] |= 1
	}
	invalid := d.held(t, "şifreli.zip", "application/zip", encrypted)

	var reported []string
	report, err := worker.Pass(context.Background(), func(err error) { reported = append(reported, err.Error()) })
	if err != nil || report.Rejected != 2 || report.Clean+report.Failed != 0 {
		t.Fatalf("report %+v, err %v", report, err)
	}
	if streams := d.clamd.Streams(); len(streams) != 0 {
		t.Fatalf("clamd read %d files the check refused", len(streams))
	}
	for _, c := range []struct {
		m    media.Media
		want media.ScanResult
	}{{tooLarge, media.ScanTooLarge}, {invalid, media.ScanArchiveInvalid}} {
		m, want := c.m, c.want
		got := d.get(t, m.ID)
		if got.Status != media.StatusRejected || got.ScanResult != want || got.BlobPurgedAt == nil {
			t.Fatalf("%s: %s (%s), purged %v; want rejected as %s", m.Name, got.Status, got.ScanResult, got.BlobPurgedAt, want)
		}
		if _, held := d.blobs.Get(m.Key); held {
			t.Fatalf("%s: the held file is still stored", m.Name)
		}
		if rejection := rejectionOf(t, d, m.ID); rejection.result != want || rejection.signature != "" {
			t.Fatalf("%s: rejection %+v", m.Name, rejection)
		}
	}
	joined := strings.Join(reported, "\n")
	if !strings.Contains(joined, "rejected as too_large_to_scan") || !strings.Contains(joined, "member 1 inflates to 2097152 bytes") ||
		!strings.Contains(joined, "rejected as archive_invalid") || !strings.Contains(joined, "member 1 is encrypted") {
		t.Fatalf("reported %q", reported)
	}
	if strings.Contains(joined, "zeros.bin") || strings.Contains(joined, "secret.txt") || strings.Contains(joined, "veri.zip") {
		t.Fatalf("a report names a file: %q", reported)
	}
	if watch.reads == 0 {
		t.Fatal("the ZIPs were not read by ranges")
	}
	for i, leased := range watch.leased {
		if leased != 0 {
			t.Fatalf("ranged read %d ran with %d database connections held", i, leased)
		}
	}
}

// A ZIP the check passes is scanned as any held file: streamed whole to
// clamd, and served once clean; one carrying malware is still found by
// clamd. The worker's limits default to the wizard's (DefaultScanLimits).
func TestPostgresScanStreamsAZIPTheCheckPassesToClamd(t *testing.T) {
	d := newScanDatabase(t)
	nested := zipOf(t, map[string][]byte{"ders.txt": bytes.Repeat([]byte("SKY LAB "), 1000)})
	clean := zipOf(t, map[string][]byte{"notlar.txt": bytes.Repeat([]byte("not "), 5000), "iç.zip": nested})
	cleanZIP := d.held(t, "notlar.zip", "application/zip", clean)

	var infected bytes.Buffer
	w := zip.NewWriter(&infected)
	f, err := w.CreateHeader(&zip.FileHeader{Name: "readme.txt", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(clamd.EICAR()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	infectedZIP := d.held(t, "virüs.zip", "application/zip", infected.Bytes())

	if report := d.pass(t); report.Clean != 1 || report.Rejected != 1 || report.Failed != 0 {
		t.Fatalf("report %+v", report)
	}
	streams := d.clamd.Streams()
	if len(streams) != 2 || !slicesContainBytes(streams, clean) || !slicesContainBytes(streams, infected.Bytes()) {
		t.Fatalf("clamd read %d files, want both ZIPs whole", len(streams))
	}
	served := d.get(t, cleanZIP.ID)
	if served.Status != media.StatusPending || served.ScanResult != media.ScanClean || served.Key != "files/"+cleanZIP.ID.String() {
		t.Fatalf("clean ZIP %s (%s) at %s", served.Status, served.ScanResult, served.Key)
	}
	if got := d.get(t, infectedZIP.ID); got.Status != media.StatusRejected || got.ScanResult != media.ScanInfected {
		t.Fatalf("infected ZIP %s (%s)", got.Status, got.ScanResult)
	}
}

func slicesContainBytes(all [][]byte, want []byte) bool {
	for _, b := range all {
		if bytes.Equal(b, want) {
			return true
		}
	}
	return false
}

// A ZIP whose held file is gone is lost, as any held file. A private ZIP
// cannot be read by ranges yet (private Direct upload, ticket 21): it is
// never scanned unchecked, but waits scanning and is tried again, until its
// scan deadline rejects it.
func TestPostgresScanOfAZIPItCannotReadWaitsOrIsLost(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	gone := d.held(t, "gitti.zip", "application/zip", zipOf(t, map[string][]byte{"a.txt": []byte("a")}))
	if err := d.blobs.Delete(ctx, gone.Key); err != nil {
		t.Fatal(err)
	}

	data := zipOf(t, map[string][]byte{"a.txt": []byte("a")})
	sealed, err := media.NewPrivateStorage(d.private, transit.New(d.bao.Config())).Seal(ctx, media.PrivateObjectKey("files/"+uuid.NewString()), data)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(24 * time.Hour)
	private, err := d.store.Create(ctx, media.Media{
		Name: "oyun.zip", Type: "application/zip", Key: sealed.Key, Size: int64(len(data)), UploadedBy: d.uploader(), Kind: media.KindFile,
		Purpose: media.PurposeAnswerFileLarge, Status: media.StatusScanning, Visibility: media.VisibilityPrivate,
		Encryption: &sealed.Encryption, ExpiresAt: &expires, CoverColors: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}

	var reported []error
	report, err := d.worker.Pass(ctx, func(err error) { reported = append(reported, err) })
	if err != nil || report.Rejected != 1 || report.Failed != 1 {
		t.Fatalf("report %+v, err %v", report, err)
	}
	if got := d.get(t, gone.ID); got.Status != media.StatusRejected || got.ScanResult != media.ScanLost {
		t.Fatalf("a ZIP whose held file is gone: %s (%s)", got.Status, got.ScanResult)
	}
	if got := d.get(t, private.ID); got.Status != media.StatusScanning || attempts(t, d, private.ID) != 1 {
		t.Fatalf("a private ZIP: %s after %d attempts", got.Status, attempts(t, d, private.ID))
	}
	if len(d.clamd.Streams()) != 0 {
		t.Fatal("clamd read a ZIP that was not checked")
	}
	if len(reported) != 1 || !errors.Is(reported[0], media.ErrPrivateZIPUnchecked) {
		t.Fatalf("reported %v", reported)
	}
}
