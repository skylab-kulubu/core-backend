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

// rangeWatch is the public bucket that notes every ranged read, and how many
// database connections the pool has handed out at each.
type rangeWatch struct {
	*media.MemoryBlob
	d      *scanDatabase
	mu     sync.Mutex
	reads  map[string]int
	leased []int32
}

func (r *rangeWatch) OpenRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	r.mu.Lock()
	if r.reads == nil {
		r.reads = map[string]int{}
	}
	r.reads[key]++
	r.leased = append(r.leased, r.d.pool.Stat().AcquiredConns())
	r.mu.Unlock()
	return r.MemoryBlob.OpenRange(ctx, key, off, n)
}

// testLimits are clamd's limits scaled down: 1 MiB a member, 4 MiB in all,
// and 8 MiB of an inner archive kept in memory.
var testLimits = media.ScanLimits{MaxFileSize: 1 << 20, MaxScanSize: 4 << 20, MaxFiles: 100, MaxRecursion: 17, MaxBuffer: 8 << 20}

// zipWorker is a scan worker on the test's bucket and clock with the
// limits given.
func (d *scanDatabase) zipWorker(t *testing.T, storage media.ScanStorage, limits media.ScanLimits) *media.ScanWorker {
	t.Helper()
	w, err := media.NewScanWorker(media.ScanWorkerConfig{
		Store: d.store, Scanner: d.client, Public: storage,
		Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config())),
		Now:     func() time.Time { return d.now },
		Limits:  limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// passWith makes one pass of the worker and collects what it reports.
func passWith(t *testing.T, w *media.ScanWorker) (media.ScanReport, []error) {
	t.Helper()
	var reported []error
	report, err := w.Pass(context.Background(), func(err error) { reported = append(reported, err) })
	if err != nil {
		t.Fatal(err)
	}
	return report, reported
}

// docxOf is a Word document (Content_Types and main part) with the other
// parts given.
func docxOf(t *testing.T, parts map[string][]byte) []byte {
	t.Helper()
	all := map[string][]byte{
		"[Content_Types].xml": []byte(`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`),
		"word/document.xml": []byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"/>`),
	}
	for name, data := range parts {
		all[name] = data
	}
	return zipOf(t, all)
}

// Before a ZIP reaches clamd, the scan worker reads it by ranges, holding no
// database connection, and rejects one clamd could not scan whole: over
// clamd's limits it is too_large_to_scan, malformed or holding what clamd
// cannot read it is archive_invalid, holding an archive core cannot check
// it is archive_nested. A ZIP is told by its content, whatever its type
// says: a file stored as a PDF that is a ZIP is checked too. None reaches
// clamd; each is rejected as any other (its held file deleted, the
// rejection recorded without a signature) and reported (ScanRejection) with
// why, by member number.
func TestPostgresScanRejectsAZIPClamdCannotScanWhole(t *testing.T) {
	d := newScanDatabase(t)
	watch := &rangeWatch{MemoryBlob: d.blobs, d: d}
	worker := d.zipWorker(t, watch, testLimits)

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
	nested := d.held(t, "yedek.zip", "application/zip", zipOf(t, map[string][]byte{"backup.bin": append([]byte("7z\xbc\xaf\x27\x1c"), make([]byte, 100)...)}))
	mislabelled := d.held(t, "rapor.pdf", "application/pdf", zipOf(t, map[string][]byte{"zeros.bin": make([]byte, 2<<20)}))

	report, reported := passWith(t, worker)
	if report.Rejected != 4 || report.Clean+report.Failed != 0 {
		t.Fatalf("report %+v", report)
	}
	if streams := d.clamd.Streams(); len(streams) != 0 {
		t.Fatalf("clamd read %d files the check refused", len(streams))
	}
	for _, c := range []struct {
		m    media.Media
		want media.ScanResult
	}{{tooLarge, media.ScanTooLarge}, {invalid, media.ScanArchiveInvalid}, {nested, media.ScanArchiveNested}, {mislabelled, media.ScanTooLarge}} {
		got := d.get(t, c.m.ID)
		if got.Status != media.StatusRejected || got.ScanResult != c.want || got.BlobPurgedAt == nil {
			t.Fatalf("%s: %s (%s), purged %v; want rejected as %s", c.m.Name, got.Status, got.ScanResult, got.BlobPurgedAt, c.want)
		}
		if _, held := d.blobs.Get(c.m.Key); held {
			t.Fatalf("%s: the held file is still stored", c.m.Name)
		}
		if rejection := rejectionOf(t, d, c.m.ID); rejection.result != c.want || rejection.signature != "" {
			t.Fatalf("%s: rejection %+v", c.m.Name, rejection)
		}
		found := false
		for _, err := range reported {
			var r *media.ScanRejection
			if errors.As(err, &r) && r.ID == c.m.ID && r.Result == c.want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: no ScanRejection reported as %s in %v", c.m.Name, c.want, reported)
		}
	}
	var joined []string
	for _, err := range reported {
		joined = append(joined, err.Error())
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{"rejected as too_large_to_scan", "member 1 inflates to 2097152 bytes", "rejected as archive_invalid",
		"member 1 is encrypted", "rejected as archive_nested", "member 1 is a 7-Zip archive"} {
		if !strings.Contains(all, want) {
			t.Fatalf("reported %q, want %q", joined, want)
		}
	}
	for _, name := range []string{"zeros.bin", "secret.txt", "veri.zip", "backup.bin", "rapor.pdf"} {
		if strings.Contains(all, name) {
			t.Fatalf("a report names a file: %q", joined)
		}
	}
	if watch.reads[tooLarge.Key] == 0 {
		t.Fatal("the ZIP was not read by ranges")
	}
	for i, leased := range watch.leased {
		if leased != 0 {
			t.Fatalf("ranged read %d ran with %d database connections held", i, leased)
		}
	}
}

// A ZIP the check passes is scanned as any held file: streamed whole to
// clamd, and served once clean; one carrying malware is still found by
// clamd. A file that is no ZIP is read by one ranged read of its first
// bytes before clamd scans it. The worker's limits default to the wizard's
// (DefaultScanLimits).
func TestPostgresScanStreamsAZIPTheCheckPassesToClamd(t *testing.T) {
	d := newScanDatabase(t)
	watch := &rangeWatch{MemoryBlob: d.blobs, d: d}
	worker := d.zipWorker(t, watch, media.ScanLimits{})
	nested := zipOf(t, map[string][]byte{"ders.txt": bytes.Repeat([]byte("SKY LAB "), 1000)})
	clean := zipOf(t, map[string][]byte{"notlar.txt": bytes.Repeat([]byte("not "), 5000), "iç.zip": nested,
		"sunum.docx": docxOf(t, nil)})
	cleanZIP := d.held(t, "notlar.zip", "application/zip", clean)
	pdf := d.held(t, "program.pdf", "application/pdf", pdfFile())

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

	if report, reported := passWith(t, worker); report.Clean != 2 || report.Rejected != 1 || report.Failed != 0 {
		t.Fatalf("report %+v %v", report, reported)
	}
	streams := d.clamd.Streams()
	if len(streams) != 3 || !slicesContainBytes(streams, clean) || !slicesContainBytes(streams, infected.Bytes()) || !slicesContainBytes(streams, pdfFile()) {
		t.Fatalf("clamd read %d files, want both ZIPs and the PDF whole", len(streams))
	}
	if watch.reads[pdf.Key] != 1 {
		t.Fatalf("the PDF was read by %d ranged reads, want one of its first bytes", watch.reads[pdf.Key])
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

// An Answer file is private, decrypted to be scanned. One that is a ZIP
// package (a DOCX) is decrypted into memory, at most MaxBuffer, and checked
// before clamd reads it: a DOCX whose part inflates past MaxFileSize, which
// clamd would pass unscanned, is rejected as too_large_to_scan. A clean one
// is scanned as before.
func TestPostgresScanChecksAPrivateDOCXFromMemory(t *testing.T) {
	d := newScanDatabase(t)
	worker := d.zipWorker(t, d.blobs, testLimits)
	ctx := context.Background()
	bomb, err := d.svc.UploadForPurpose(ctx, d.organizer, media.PurposeAnswerFile,
		uploaded("cv.docx", "application/octet-stream", docxOf(t, map[string][]byte{"word/media/image1.bin": make([]byte, 2<<20)})))
	if err != nil {
		t.Fatal(err)
	}
	clean, err := d.svc.UploadForPurpose(ctx, d.organizer, media.PurposeAnswerFile, uploaded("ödev.docx", "application/octet-stream", docxOf(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if report, reported := passWith(t, worker); report.Rejected != 1 || report.Clean != 1 || report.Failed != 0 {
		t.Fatalf("report %+v %v", report, reported)
	}
	if got := d.get(t, bomb.ID); got.Status != media.StatusRejected || got.ScanResult != media.ScanTooLarge {
		t.Fatalf("the DOCX bomb: %s (%s)", got.Status, got.ScanResult)
	}
	if _, ok := d.private.Get(bomb.Key); ok {
		t.Fatal("the DOCX bomb's object is still stored")
	}
	if streams := d.clamd.Streams(); len(streams) != 1 {
		t.Fatalf("clamd read %d files, want only the clean DOCX", len(streams))
	}
	if got := d.get(t, clean.ID); got.ScanResult != media.ScanClean {
		t.Fatalf("the clean DOCX: %s (%s)", got.Status, got.ScanResult)
	}
}

// A ZIP whose held file is gone is lost, as any held file. A private ZIP
// larger than MaxBuffer cannot be checked from memory, nor read by ranges
// until private Direct upload (ticket 21): it is never scanned unchecked,
// but waits scanning and is tried again, until its scan deadline rejects it.
func TestPostgresScanOfAZIPItCannotReadWaitsOrIsLost(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	gone := d.held(t, "gitti.zip", "application/zip", zipOf(t, map[string][]byte{"a.txt": []byte("a")}))
	if err := d.blobs.Delete(ctx, gone.Key); err != nil {
		t.Fatal(err)
	}

	data := zipOf(t, map[string][]byte{"a.bin": bytes.Repeat([]byte("oyun "), 1000)})
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

	little := testLimits
	little.MaxBuffer = int64(len(data)) - 1
	report, reported := passWith(t, d.zipWorker(t, d.blobs, little))
	if report.Rejected != 1 || report.Failed != 1 {
		t.Fatalf("report %+v", report)
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

// mustScanWorker is a scan worker, or the test's end.
func mustScanWorker(t *testing.T, config media.ScanWorkerConfig) *media.ScanWorker {
	t.Helper()
	w, err := media.NewScanWorker(config)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
