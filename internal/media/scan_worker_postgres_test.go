package media_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/clamd/clamdtest"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// scanDatabase is privateDatabase with the reviewed catalogue, so an Answer
// file needs its scan, a fake clamd, and the scan worker on a clock the
// test moves.
type scanDatabase struct {
	privateDatabase
	clamd  *clamdtest.Server
	client *clamd.Client
	worker *media.ScanWorker
	now    time.Time
	// midCopy runs while a test's copy is in flight (midCopyStorage).
	midCopy func()
}

func newScanDatabase(t *testing.T) *scanDatabase {
	t.Helper()
	pd := newPrivateDatabase(t)
	fake := clamdtest.New(t)
	d := &scanDatabase{clamd: fake, client: clamd.New(fake.Addr()), now: time.Now().UTC()}
	d.worker = mustScanWorker(t, media.ScanWorkerConfig{
		Store: pd.store, Scanner: d.client, Public: pd.blobs,
		Private: media.NewPrivateStorage(pd.private, transit.New(pd.bao.Config())),
		Now:     func() time.Time { return d.now },
	})
	pd.svc = media.NewServiceWithOptions(pd.store, pd.blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: authz.ServiceProducts,
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(pd.private, transit.New(pd.bao.Config())),
				LinkKey: bytes.Repeat([]byte{9}, 32), LinkOrigin: "https://api.example.test",
				AccessLog: pd.store, Now: pd.bao.Clock.Now,
			},
			Scans: d.worker,
		})
	d.privateDatabase = pd
	return d
}

// answer uploads an Answer file of the organizer.
func (d *scanDatabase) answer(t *testing.T, data []byte) media.Media {
	t.Helper()
	created, err := d.svc.UploadForPurpose(context.Background(), d.organizer, media.PurposeAnswerFile, uploaded("cv.pdf", "application/pdf", data))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != media.StatusScanning {
		t.Fatalf("uploaded %s, want scanning", created.Status)
	}
	return created
}

// pass makes one scan pass and fails the test on an error that stopped it.
func (d *scanDatabase) pass(t *testing.T) media.ScanReport {
	t.Helper()
	report, err := d.worker.Pass(context.Background(), func(err error) { t.Logf("scan: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func infectedPDF() []byte {
	return append(pdfFile(), clamd.EICAR()...)
}

// A clean Answer file is scanned through decryption: clamd reads its
// plaintext (only ciphertext is stored), and the Media becomes pending,
// keeping its expiry, with its scan result.
func TestPostgresScanDecryptsAPrivateMediaAndMovesItOnWhenClean(t *testing.T) {
	d := newScanDatabase(t)
	created := d.answer(t, pdfFile())
	if stored, _ := d.private.Get(created.Key); bytes.Contains(stored, pdfFile()) {
		t.Fatal("the private object holds the plaintext")
	}

	if report := d.pass(t); report.Clean != 1 || report.Rejected+report.Failed != 0 {
		t.Fatalf("report %+v", report)
	}
	streams := d.clamd.Streams()
	if len(streams) != 1 || !bytes.Equal(streams[0], pdfFile()) {
		t.Fatalf("clamd read %q, want the plaintext", streams)
	}
	got := d.get(t, created.ID)
	if got.Status != media.StatusPending || got.ScanResult != media.ScanClean || got.ExpiresAt == nil || !got.ExpiresAt.Equal(*created.ExpiresAt) {
		t.Fatalf("scanned %s (%s), expires %v, want pending with its expiry %v", got.Status, got.ScanResult, got.ExpiresAt, created.ExpiresAt)
	}
	if got.Key != created.Key {
		t.Fatalf("a private Media moved from %s to %s", created.Key, got.Key)
	}
	// Nothing is left to do.
	if report := d.pass(t); report.Clean != 0 || len(d.clamd.Streams()) != 1 {
		t.Fatalf("second pass %+v", report)
	}
}

// An Answer file can be attached while it waits for its scan (a Skyforms
// draft holds it): it stays scanning, without expiry, and becomes attached
// once clean. Removed from its record while scanning, it expires 30 days
// later, as a detached Media does.
func TestPostgresAnAnswerFileIsAttachedWhileScanning(t *testing.T) {
	d := newScanDatabase(t)
	kept := d.answer(t, pdfFile())
	draft := media.Owner{Service: authz.ProductForms, Type: "draft", ID: uuid.NewString()}
	d.attachFor(t, formsService, kept, draft, media.RoleFormsAnswer)
	if got := d.get(t, kept.ID); got.Status != media.StatusScanning || got.ExpiresAt != nil {
		t.Fatalf("attached while scanning: %s expires %v, want scanning without expiry", got.Status, got.ExpiresAt)
	}

	dropped := d.answer(t, pdfFile())
	link := d.attachFor(t, formsService, dropped, draft, media.RoleFormsAnswer)
	before := time.Now()
	if err := d.svc.Detach(context.Background(), formsService, dropped.ID, link.ID); err != nil {
		t.Fatal(err)
	}
	got := d.get(t, dropped.ID)
	window := 30 * 24 * time.Hour
	if got.Status != media.StatusScanning || got.ExpiresAt == nil || got.ExpiresAt.Before(before.Add(window-clockSkew)) || got.ExpiresAt.After(time.Now().Add(window+clockSkew)) {
		t.Fatalf("detached while scanning: %s expires %v, want scanning for 30 days", got.Status, got.ExpiresAt)
	}

	if report := d.pass(t); report.Clean != 2 {
		t.Fatalf("report %+v", report)
	}
	attached(t, d.get(t, kept.ID))
	if got := d.get(t, kept.ID); got.ScanResult != media.ScanClean {
		t.Fatalf("scan result %q", got.ScanResult)
	}
	if got := d.get(t, dropped.ID); got.Status != media.StatusPending || got.ExpiresAt == nil {
		t.Fatalf("clean and unattached: %s expires %v", got.Status, got.ExpiresAt)
	}
}

// An infected Answer file is rejected: its object is deleted from the
// private bucket, the rejection is recorded (Media, reason, signature,
// time; no file name), and its owning product sees why on its metadata.
// Nothing can link it any more.
func TestPostgresScanRejectsAnInfectedFileAndDeletesIt(t *testing.T) {
	d := newScanDatabase(t)
	infected := d.answer(t, infectedPDF())
	response := media.Owner{Service: authz.ProductForms, Type: "response", ID: uuid.NewString()}
	d.attachFor(t, formsService, infected, response, media.RoleFormsAnswer)

	if report := d.pass(t); report.Rejected != 1 || report.Clean+report.Failed != 0 {
		t.Fatalf("report %+v", report)
	}
	if _, ok := d.private.Get(infected.Key); ok {
		t.Fatal("the infected object is still stored")
	}
	got := d.get(t, infected.ID)
	if got.Status != media.StatusRejected || got.ScanResult != media.ScanInfected || got.BlobPurgedAt == nil || got.ExpiresAt != nil {
		t.Fatalf("rejected Media %s (%s), purged %v, expires %v", got.Status, got.ScanResult, got.BlobPurgedAt, got.ExpiresAt)
	}
	rejection := rejectionOf(t, d, infected.ID)
	if rejection.result != media.ScanInfected || rejection.signature != clamdtest.Signature ||
		rejection.at.Sub(d.now).Abs() > time.Millisecond {
		t.Fatalf("rejection %+v", rejection)
	}
	var columns []string
	rows, err := d.pool.Query(context.Background(), `SELECT column_name FROM information_schema.columns WHERE table_name = 'media_scan_rejections' ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if want := "media_id rejected_at result signature"; strings.Join(columns, " ") != want {
		t.Fatalf("the rejection record keeps %v, want %s", columns, want)
	}

	seen, err := d.svc.Get(context.Background(), formsService, infected.ID)
	if err != nil || seen.Status != media.StatusRejected || seen.ScanResult != media.ScanInfected {
		t.Fatalf("Skyforms sees %s (%s), err %v", seen.Status, seen.ScanResult, err)
	}
	if _, _, err := d.svc.Attach(context.Background(), formsService, infected.ID, media.AttachRequest{
		Owner: media.Owner{Service: authz.ProductForms, Type: "response", ID: uuid.NewString()}, Role: media.RoleFormsAnswer, OnBehalfOf: d.uploader(),
	}); !errors.Is(err, media.ErrNotLinkable) {
		t.Fatalf("linking a rejected Media: err = %v, want %v", err, media.ErrNotLinkable)
	}
}

// A file longer than clamd takes in one stream cannot be scanned whole: it
// is rejected as too large to scan. So is one clamd could only scan in
// part (a Heuristics.Limits.Exceeded signature, with AlertExceedsMax).
func TestPostgresScanRejectsAFileTooLargeToScan(t *testing.T) {
	d := newScanDatabase(t)
	long := d.answer(t, append(pdfFile(), bytes.Repeat([]byte(" "), 4096)...))
	d.clamd.LimitStream(1024)
	if report := d.pass(t); report.Rejected != 1 {
		t.Fatalf("report %+v", report)
	}
	if got := d.get(t, long.ID); got.Status != media.StatusRejected || got.ScanResult != media.ScanTooLarge {
		t.Fatalf("%s (%s), want rejected as too large to scan", got.Status, got.ScanResult)
	}
	if rejection := rejectionOf(t, d, long.ID); rejection.result != media.ScanTooLarge || rejection.signature != "" {
		t.Fatalf("rejection %+v", rejection)
	}

	d.clamd.LimitStream(0)
	marker := []byte("%%nested-archive-bomb")
	d.clamd.Report(marker, "Heuristics.Limits.Exceeded.MaxFiles")
	partial := d.answer(t, append(pdfFile(), marker...))
	if report := d.pass(t); report.Rejected != 1 {
		t.Fatalf("report %+v", report)
	}
	if rejection := rejectionOf(t, d, partial.ID); rejection.result != media.ScanTooLarge || rejection.signature != "Heuristics.Limits.Exceeded.MaxFiles" {
		t.Fatalf("rejection %+v", rejection)
	}
}

// While clamd cannot be reached, the pass stops and the Media waits
// scanning, with no failure counted against it; once clamd answers, it is
// scanned.
func TestPostgresScanWaitsWhileTheScannerIsDown(t *testing.T) {
	d := newScanDatabase(t)
	waiting := d.answer(t, pdfFile())
	d.clamd.Stop()

	_, err := d.worker.Pass(context.Background(), func(err error) { t.Logf("scan: %v", err) })
	if !errors.Is(err, media.ErrScannerDown) || !errors.Is(err, clamd.ErrUnreachable) {
		t.Fatalf("err = %v, want %v", err, media.ErrScannerDown)
	}
	if got := d.get(t, waiting.ID); got.Status != media.StatusScanning || attempts(t, d, waiting.ID) != 0 {
		t.Fatalf("while down: %s after %d failures", got.Status, attempts(t, d, waiting.ID))
	}

	back := clamdtest.New(t)
	d.client.Addr = back.Addr()
	if report := d.pass(t); report.Clean != 1 {
		t.Fatalf("report %+v", report)
	}
}

func attempts(t *testing.T, d *scanDatabase, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(), `SELECT scan_attempts FROM media WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A Media whose scan fails (clamd answers an error, its object cannot be
// read) waits scanning and is tried again later, each failure in a row
// doubling the wait; the pass walks past it to the others.
func TestPostgresScanRetriesAFailedMediaLaterAndWalksPastIt(t *testing.T) {
	d := newScanDatabase(t)
	unreadable, clean := d.answer(t, pdfFile()), d.answer(t, pdfFile())
	flaky := &failingOpens{MemoryBlob: d.private, key: unreadable.Key}
	d.worker = mustScanWorker(t, media.ScanWorkerConfig{
		Store: d.store, Scanner: d.client, Public: d.blobs,
		Private: media.NewPrivateStorage(flaky, transit.New(d.bao.Config())),
		Now:     func() time.Time { return d.now },
	})
	if report := d.pass(t); report.Clean != 1 || report.Failed != 1 {
		t.Fatalf("report %+v, want the readable one clean past the other", report)
	}
	if got := d.get(t, clean.ID); got.Status != media.StatusPending {
		t.Fatalf("readable one %s", got.Status)
	}
	if got := d.get(t, unreadable.ID); got.Status != media.StatusScanning || attempts(t, d, unreadable.ID) != 1 {
		t.Fatalf("after a failure: %s, %d attempts", got.Status, attempts(t, d, unreadable.ID))
	}
	flaky.key = ""
	// Tried again only once its wait (30 seconds) is over.
	if report := d.pass(t); report.Clean+report.Failed != 0 {
		t.Fatalf("scanned before its retry time: %+v", report)
	}
	d.now = d.now.Add(31 * time.Second)
	if report := d.pass(t); report.Clean != 1 {
		t.Fatalf("report %+v", report)
	}

	// clamd answers an error: the wait doubles, 30 seconds, then a minute.
	third := d.answer(t, pdfFile())
	d.clamd.Fail("stream: Can't allocate memory")
	d.pass(t)
	d.now = d.now.Add(31 * time.Second)
	d.pass(t)
	if got := attempts(t, d, third.ID); got != 2 {
		t.Fatalf("%d attempts, want 2", got)
	}
	d.clamd.Fail("")
	d.now = d.now.Add(31 * time.Second)
	if report := d.pass(t); report.Clean != 0 {
		t.Fatalf("scanned before its doubled wait: %+v", report)
	}
	d.now = d.now.Add(30 * time.Second)
	if report := d.pass(t); report.Clean != 1 {
		t.Fatalf("report %+v", report)
	}
	if attempts(t, d, third.ID) != 0 {
		t.Fatal("a clean Media kept its failures")
	}
}

// A rejection whose object could not be deleted is finished by a later
// pass: the Media stays rejected, and its object goes.
func TestPostgresScanFinishesARejectionCutShort(t *testing.T) {
	d := newScanDatabase(t)
	infected := d.answer(t, infectedPDF())
	failing := &failingDeletes{MemoryBlob: d.private, fail: true}
	worker := mustScanWorker(t, media.ScanWorkerConfig{
		Store: d.store, Scanner: d.client,
		Private: media.NewPrivateStorage(failing, transit.New(d.bao.Config())),
		Now:     func() time.Time { return d.now },
	})
	report, err := worker.Pass(context.Background(), func(err error) { t.Logf("scan: %v", err) })
	if err != nil || report.Rejected != 1 || report.Failed != 1 {
		t.Fatalf("report %+v, err %v", report, err)
	}
	if got := d.get(t, infected.ID); got.Status != media.StatusRejected || got.BlobPurgedAt != nil || got.BlobPurgeStartedAt == nil {
		t.Fatalf("cut short: %s, purge started %v, purged %v", got.Status, got.BlobPurgeStartedAt, got.BlobPurgedAt)
	}
	if _, ok := d.private.Get(infected.Key); !ok {
		t.Fatal("the object is gone although its delete failed")
	}

	failing.fail = false
	d.now = d.now.Add(time.Minute)
	report, err = worker.Pass(context.Background(), func(err error) { t.Logf("scan: %v", err) })
	if err != nil || report.Finished != 1 {
		t.Fatalf("report %+v, err %v", report, err)
	}
	if _, ok := d.private.Get(infected.Key); ok {
		t.Fatal("the object is still stored")
	}
	if got := d.get(t, infected.ID); got.Status != media.StatusRejected || got.BlobPurgedAt == nil {
		t.Fatalf("finished: %s, purged %v", got.Status, got.BlobPurgedAt)
	}
	if len(d.clamd.Streams()) != 1 {
		t.Fatal("a rejected Media was scanned again")
	}
}

type failingDeletes struct {
	*media.MemoryBlob
	fail bool
}

func (f *failingDeletes) Delete(ctx context.Context, key string) error {
	if f.fail {
		return errors.New("storage: delete failed")
	}
	return f.MemoryBlob.Delete(ctx, key)
}

// The expiry cleanup treats a Media waiting for its scan as a pending one:
// past its expiry with nothing keeping it, it is purged, and the scan has
// nothing left to do with it.
func TestPostgresExpiryPurgesAMediaStillScanning(t *testing.T) {
	d := newScanDatabase(t)
	stale := d.answer(t, pdfFile())
	d.clamd.Stop()

	report, err := media.PurgeExpired(context.Background(), d.store, media.Buckets{Public: d.blobs, Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config()))},
		stale.ExpiresAt.Add(time.Second), func(err error) { t.Fatal(err) })
	if err != nil || report.Purged != 1 {
		t.Fatalf("expiry report %+v, err %v", report, err)
	}
	if _, ok := d.private.Get(stale.Key); ok {
		t.Fatal("the expired object is still stored")
	}
	if report := d.pass(t); report.Clean+report.Rejected+report.Failed != 0 {
		t.Fatalf("the scan still had work for a purged Media: %+v", report)
	}
}

// An Answer file is opened only once clean: no read link while it waits
// for its scan, one that opens its plaintext after. Content is refused for a
// Media that is not clean, whatever link is shown.
func TestPostgresAnAnswerFileOpensOnlyOnceClean(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	created := d.answer(t, pdfFile())
	if _, err := d.svc.IssueReadLink(ctx, formsService, created.ID, forPerson(reviewer.String())); !errors.Is(err, media.ErrMediaScanning) {
		t.Fatalf("link while scanning: err = %v, want %v", err, media.ErrMediaScanning)
	}

	d.pass(t)
	link, err := d.svc.IssueReadLink(ctx, formsService, created.ID, forPerson(reviewer.String()))
	if err != nil {
		t.Fatal(err)
	}
	content, err := d.svc.OpenContent(ctx, created.ID, tokenOf(t, link), "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, content); !bytes.Equal(got, pdfFile()) {
		t.Fatalf("opened %q", got)
	}

	// The same link, for a Media no longer clean (as if a rescan rejected
	// it), opens nothing.
	if _, err := d.pool.Exec(ctx, `UPDATE media SET status = 'rejected', scan_result = 'infected' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	var refusal *media.ScanRefusal
	if _, err := d.svc.OpenContent(ctx, created.ID, tokenOf(t, link), "203.0.113.9"); !errors.Is(err, media.ErrMediaRejected) ||
		!errors.As(err, &refusal) || refusal.Result != media.ScanInfected {
		t.Fatalf("content of a rejected Media: err = %v", err)
	}
}

// heldClubFile stores a club file as a Direct upload's completion leaves
// one that needs its scan: held under pending/scan/ as an opaque download,
// waiting scanning.
func (d *scanDatabase) heldClubFile(t *testing.T, name string, data []byte) media.Media {
	t.Helper()
	return d.held(t, name, "application/pdf", data)
}

// held stores a club file of the content type as heldClubFile does.
func (d *scanDatabase) held(t *testing.T, name, contentType string, data []byte) media.Media {
	t.Helper()
	ctx := context.Background()
	key := "pending/scan/" + uuid.NewString()
	if err := d.blobs.Put(ctx, key, data, media.BlobMetadata{ContentType: "application/octet-stream", ContentDisposition: "attachment"}); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(24 * time.Hour)
	created, err := d.store.Create(ctx, media.Media{
		Name: name, Type: contentType, Key: key, Size: int64(len(data)), UploadedBy: d.uploader(), Kind: media.KindFile,
		Purpose: media.PurposeClubFile, Status: media.StatusScanning, Visibility: media.VisibilityPublic, ExpiresAt: &expires,
		CoverColors: []string{}, ServingPolicyApplied: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// Account erasure takes the person's uploads whatever their scan state
// (#133's rule, unchanged): an Answer file waiting for its scan is purged
// at once; a club file waiting for it keeps its file, without the person's
// name, and stays held unserved until the scan finds it clean (the
// erasure's metadata rewrite only ever makes a nameless download, which a
// held file already is); a rejected one has nothing left to erase.
func TestPostgresAccountErasureTakesUploadsWhateverTheirScan(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	rejected := d.answer(t, infectedPDF())
	d.pass(t)
	answer := d.answer(t, pdfFile())
	club := d.heldClubFile(t, "Ada_Organizer_dataset.pdf", pdfFile())

	users := user.NewPostgresStore(d.pool)
	if _, err := users.RequestDeletion(ctx, d.uploader(), nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, d.uploader(), time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	eraser := media.NewImmediateBlobEraser(d.store, media.Buckets{Public: d.blobs, Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config()))})
	for _, item := range []media.Media{rejected, answer, club} {
		if err := eraser.EnsureErased(ctx, item.ID, time.Now().UTC()); err != nil {
			t.Fatalf("erase %s: %v", item.Purpose, err)
		}
	}
	var left int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM account_deletion_media`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d uploads left to erase, err %v", left, err)
	}

	if got := d.get(t, answer.ID); got.BlobPurgedAt == nil {
		t.Fatalf("the scanning Answer file was kept: %s", got.Status)
	}
	if _, ok := d.private.Get(answer.Key); ok {
		t.Fatal("the scanning Answer file's object is still stored")
	}
	kept := d.get(t, club.ID)
	if kept.Status != media.StatusScanning || kept.BlobPurgedAt != nil || kept.Name != "" || kept.UploadedBy != uuid.Nil {
		t.Fatalf("club file %s, purged %v, name %q, uploader %v", kept.Status, kept.BlobPurgedAt, kept.Name, kept.UploadedBy)
	}
	if meta, _ := d.blobs.Metadata(club.Key); meta.ContentType != "application/octet-stream" || meta.ContentDisposition != "attachment" {
		t.Fatalf("the held club file is stored as %+v, want it opaque until clean", meta)
	}

	if report := d.pass(t); report.Clean != 1 {
		t.Fatalf("report %+v", report)
	}
	served := d.get(t, club.ID)
	meta, ok := d.blobs.Metadata(served.Key)
	if served.Status != media.StatusPending || served.Key != "files/"+club.ID.String() || !ok || meta.ContentType != "application/pdf" ||
		strings.Contains(meta.ContentDisposition, "Ada_Organizer") {
		t.Fatalf("clean club file %s at %s stored as %+v", served.Status, served.Key, meta)
	}
}

// Restoring an archived Media waiting for its scan starts its expiry again
// only when nothing keeps it: one a Media attachment links keeps none, as
// an attached Media does.
func TestPostgresRestoringAnAttachedScanningMediaGivesItNoExpiry(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	kept := d.answer(t, pdfFile())
	d.attachFor(t, formsService, kept, media.Owner{Service: authz.ProductForms, Type: "draft", ID: uuid.NewString()}, media.RoleFormsAnswer)
	loose := d.answer(t, pdfFile())
	for _, item := range []media.Media{kept, loose} {
		if err := d.svc.Delete(ctx, d.organizer, item.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := d.svc.Restore(ctx, d.organizer, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := d.get(t, kept.ID); got.Status != media.StatusScanning || got.ExpiresAt != nil {
		t.Fatalf("restored while attached: %s expires %v, want scanning with no expiry", got.Status, got.ExpiresAt)
	}
	if got := d.get(t, loose.ID); got.Status != media.StatusScanning || got.ExpiresAt == nil {
		t.Fatalf("restored with nothing keeping it: %s expires %v, want scanning with an expiry", got.Status, got.ExpiresAt)
	}
}

// rejection is the scan's record of one rejection.
type rejection struct {
	result    media.ScanResult
	signature string
	at        time.Time
}

func rejectionOf(t *testing.T, d *scanDatabase, id uuid.UUID) rejection {
	t.Helper()
	var r rejection
	if err := d.pool.QueryRow(context.Background(), `SELECT result, signature, rejected_at FROM media_scan_rejections WHERE media_id = $1`, id).
		Scan(&r.result, &r.signature, &r.at); err != nil {
		t.Fatalf("rejection of %s: %v", id, err)
	}
	return r
}

// failingOpens is a bucket whose reads of key fail, as a storage outage
// does (not a missing object).
type failingOpens struct {
	*media.MemoryBlob
	key string
}

func (f *failingOpens) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == f.key {
		return nil, errors.New("r2: 503 Service Unavailable")
	}
	return f.MemoryBlob.Open(ctx, key)
}
