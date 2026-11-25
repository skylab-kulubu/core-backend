package media_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
)

// recordingPublic is the public bucket of a test: what it deleted (each
// delete queues a CDN purge in core), and deletes made to fail.
type recordingPublic struct {
	*media.MemoryBlob
	deleted     []string
	failDeletes int
}

func (p *recordingPublic) Delete(ctx context.Context, key string) error {
	if p.failDeletes > 0 {
		p.failDeletes--
		return errors.New("storage is down")
	}
	p.deleted = append(p.deleted, key)
	return p.MemoryBlob.Delete(ctx, key)
}

func (d *scanDatabase) legacyAnswerStorage(public *recordingPublic) media.LegacyAnswerStorage {
	return media.LegacyAnswerStorage{Public: public, Private: media.NewPrivateStorage(d.private, transit.New(d.bao.Config()))}
}

func (d *scanDatabase) runLegacyAnswers(t *testing.T, storage media.LegacyAnswerStorage, pairs []media.LegacyAnswerPair, mode media.LegacyAnswerMode, limit int) media.LegacyAnswerReport {
	t.Helper()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	report, err := media.MoveLegacyAnswers(context.Background(), d.store, storage, catalogue, pairs, mode, limit,
		func() time.Time { return d.now }, func(err error) { t.Logf("move: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func (d *scanDatabase) moveLegacyAnswers(t *testing.T, public *recordingPublic, pairs []media.LegacyAnswerPair, mode media.LegacyAnswerMode, limit int) media.LegacyAnswerReport {
	t.Helper()
	return d.runLegacyAnswers(t, d.legacyAnswerStorage(public), pairs, mode, limit)
}

// storedLegacyAnswer is an Answer file Skyforms uploaded before Media
// purpose: a legacy, public Media nothing attaches.
func (d *scanDatabase) storedLegacyAnswer(t *testing.T, data []byte) media.Media {
	t.Helper()
	created, err := d.svc.Upload(context.Background(), d.organizer, "cv.pdf", "application/pdf", data)
	if err != nil {
		t.Fatal(err)
	}
	if created.Purpose != media.PurposeLegacy || created.Visibility != media.VisibilityPublic {
		t.Fatalf("stored %s %s, want a public legacy Media", created.Purpose, created.Visibility)
	}
	return created
}

func formsResponseLinks(t *testing.T, d *scanDatabase, id uuid.UUID) []string {
	t.Helper()
	rows, err := d.pool.Query(context.Background(), `SELECT owner_id FROM media_attachments
		WHERE media_id = $1 AND owner_service = 'forms' AND owner_type = 'response' AND role = 'answer' ORDER BY owner_id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
	}
	return owners
}

// The dry run only counts, by what each Media is. A move takes the legacy
// Answer file alone: encrypted in the private bucket at private/<its key>,
// an answer_file waiting for its scan with no expiry, linked to both of its
// responses for Skyforms, the move recorded, and its public object kept
// until the scan ends. Clean and attached, finalize deletes the public
// object through the public bucket (which queues the CDN purge). A further
// run finds it done and changes nothing.
func TestPostgresLegacyAnswersMoveOnlySkyformsLegacyAnswerFiles(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	public := &recordingPublic{MemoryBlob: d.blobs}
	answer := d.storedLegacyAnswer(t, pdfFile())
	cover := d.storedBeforePurposes(t, "cover.png")
	if _, err := d.events().Create(ctx, d.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID}); err != nil {
		t.Fatal(err)
	}
	purposed := d.answer(t, pdfFile())
	first, second := uuid.New(), uuid.New()
	pairs := []media.LegacyAnswerPair{
		{MediaID: answer.ID, ResponseID: first}, {MediaID: answer.ID, ResponseID: second}, {MediaID: answer.ID, ResponseID: first},
		{MediaID: cover.ID, ResponseID: uuid.New()},
		{MediaID: purposed.ID, ResponseID: uuid.New()},
		{MediaID: uuid.New(), ResponseID: uuid.New()},
	}

	dry := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersCount, 25)

	if dry.Pairs != 6 || dry.Media != 4 || dry.ToMove != 1 || dry.ToMoveBytes != answer.Size || dry.Shared != 1 || dry.NotLegacy != 1 || dry.NotFound != 1 ||
		dry.MovedNow+dry.FinalizedNow+dry.Failed+dry.Remaining != 0 {
		t.Fatalf("dry run %+v", dry)
	}
	if got := d.get(t, answer.ID); got.Purpose != media.PurposeLegacy || got.Key != answer.Key || len(public.deleted) != 0 {
		t.Fatalf("the dry run changed the Media (%s at %s) or deleted %v", got.Purpose, got.Key, public.deleted)
	}

	applied := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersMove, 25)

	if applied.ToMove != 1 || applied.MovedNow != 1 || applied.Failed+applied.Changed+applied.Remaining+applied.FinalizedNow != 0 {
		t.Fatalf("move %+v", applied)
	}
	got := d.get(t, answer.ID)
	if got.Purpose != media.PurposeAnswerFile || got.Visibility != media.VisibilityPrivate || got.Encryption == nil ||
		got.Key != media.PrivateObjectKey(answer.Key) || got.Status != media.StatusScanning || got.ExpiresAt != nil ||
		got.Name != answer.Name || got.UploadedBy != answer.UploadedBy || !got.CreatedAt.Equal(answer.CreatedAt) {
		t.Fatalf("moved %+v", got)
	}
	if _, ok := d.blobs.Get(answer.Key); !ok || len(public.deleted) != 0 {
		t.Fatalf("the move deleted the public object before the scan (deleted %v)", public.deleted)
	}
	stored, ok := d.private.Get(got.Key)
	if !ok || bytes.Contains(stored, pdfFile()) {
		t.Fatalf("private object present %v, holds the plaintext %v", ok, ok && bytes.Contains(stored, pdfFile()))
	}
	wantOwners := []string{first.String(), second.String()}
	slices.Sort(wantOwners)
	if owners := formsResponseLinks(t, d, answer.ID); !slices.Equal(owners, wantOwners) {
		t.Fatalf("Skyforms links %v, want %v", owners, wantOwners)
	}
	if c := d.get(t, cover.ID); c.Visibility != media.VisibilityPublic || c.Key != cover.Key {
		t.Fatalf("the Event cover was moved: %+v", c)
	}

	// Before the scan, finalize deletes nothing.
	if early := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersFinalize, 25); early.AwaitingScan != 1 || early.FinalizedNow != 0 || len(public.deleted) != 0 {
		t.Fatalf("finalize before the scan %+v, deleted %v", early, public.deleted)
	}
	// The scan takes the moved file and the purposed upload.
	if report := d.pass(t); report.Clean != 2 {
		t.Fatalf("scan %+v", report)
	}

	final := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersFinalize, 25)

	if final.ReadyToFinalize != 1 || final.FinalizedNow != 1 || final.Failed != 0 {
		t.Fatalf("finalize %+v", final)
	}
	if _, ok := d.blobs.Get(answer.Key); ok || !slices.Contains(public.deleted, answer.Key) {
		t.Fatalf("public object still there, or not deleted through the public bucket (deleted %v)", public.deleted)
	}
	var publicKey string
	var deleted bool
	if err := d.pool.QueryRow(ctx, `SELECT public_key, public_deleted_at IS NOT NULL FROM media_legacy_answer_moves WHERE media_id = $1`, answer.ID).
		Scan(&publicKey, &deleted); err != nil || publicKey != answer.Key || !deleted {
		t.Fatalf("move record %q deleted %v (%v)", publicKey, deleted, err)
	}
	for _, mode := range []media.LegacyAnswerMode{media.LegacyAnswersMove, media.LegacyAnswersFinalize} {
		if again := d.moveLegacyAnswers(t, public, pairs, mode, 25); again.Moved != 1 || again.ToMove+again.MovedNow+again.FinalizedNow+again.Failed != 0 {
			t.Fatalf("mode %d again %+v", mode, again)
		}
	}
}

// A moved Answer file is scanned by the scan worker like any other, its
// deadline counted from the move: one uploaded months ago is not rejected
// as scan_timeout. Clean, it is attached and Skyforms gets a read link for
// its reviewer; infected, it is rejected, its private object deleted, and
// finalize deletes its public object too.
func TestPostgresMovedLegacyAnswersAreScannedFromTheirMove(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	public := &recordingPublic{MemoryBlob: d.blobs}
	clean := d.storedLegacyAnswer(t, pdfFile())
	infected := d.storedLegacyAnswer(t, infectedPDF())
	for _, id := range []uuid.UUID{clean.ID, infected.ID} {
		if _, err := d.pool.Exec(ctx, `UPDATE media SET created_at = $2 WHERE id = $1`, id, d.now.Add(-200*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	pairs := []media.LegacyAnswerPair{{MediaID: clean.ID, ResponseID: uuid.New()}, {MediaID: infected.ID, ResponseID: uuid.New()}}
	if report := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersMove, 25); report.MovedNow != 2 {
		t.Fatalf("move %+v", report)
	}

	d.now = d.now.Add(24 * time.Hour)
	report := d.pass(t)

	if report.Clean != 1 || report.Rejected != 1 {
		t.Fatalf("scan %+v, want one clean and one rejected (infected), none timed out", report)
	}
	if got := d.get(t, clean.ID); got.Status != media.StatusAttached || got.ScanResult != media.ScanClean {
		t.Fatalf("clean: %s (%s), want attached", got.Status, got.ScanResult)
	}
	bad := d.get(t, infected.ID)
	if bad.Status != media.StatusRejected || bad.ScanResult != media.ScanInfected {
		t.Fatalf("infected: %s (%s)", bad.Status, bad.ScanResult)
	}
	if _, ok := d.private.Get(bad.Key); ok {
		t.Fatal("the infected file's private object is still there")
	}
	if _, err := d.svc.IssueReadLink(ctx, formsService, clean.ID, forPerson(reviewer.String())); err != nil {
		t.Fatalf("Skyforms' read link: %v", err)
	}
	if _, err := d.svc.IssueReadLink(ctx, formsService, infected.ID, forPerson(reviewer.String())); err == nil {
		t.Fatal("a read link to the rejected file")
	}
	if final := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersFinalize, 25); final.FinalizedNow != 2 || final.PublicKept != 0 {
		t.Fatalf("finalize %+v, want both public objects deleted (clean and infected)", final)
	}
	if _, ok := d.blobs.Get(infected.Key); ok {
		t.Fatal("the infected file is still public")
	}
}

// A moved file the scan rejects for a reason other than malware (here its
// scan timed out) loses its private copy; its public object is then its
// only copy, and finalize keeps it.
func TestPostgresLegacyAnswersKeepThePublicCopyOfAFileRejectedForAnotherReason(t *testing.T) {
	d := newScanDatabase(t)
	public := &recordingPublic{MemoryBlob: d.blobs}
	answer := d.storedLegacyAnswer(t, pdfFile())
	pairs := []media.LegacyAnswerPair{{MediaID: answer.ID, ResponseID: uuid.New()}}
	if report := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersMove, 25); report.MovedNow != 1 {
		t.Fatalf("move %+v", report)
	}
	d.now = d.now.Add(8 * 24 * time.Hour)
	d.pass(t)
	if got := d.get(t, answer.ID); got.Status != media.StatusRejected || got.ScanResult != media.ScanTimeout {
		t.Fatalf("after the deadline: %s (%s), want rejected scan_timeout", got.Status, got.ScanResult)
	}

	final := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersFinalize, 25)

	if final.PublicKept != 1 || final.FinalizedNow != 0 || len(public.deleted) != 0 {
		t.Fatalf("finalize %+v, deleted %v", final, public.deleted)
	}
	if stored, ok := d.blobs.Get(answer.Key); !ok || !bytes.Equal(stored, pdfFile()) {
		t.Fatal("the only copy of the file is gone")
	}
}

// tamperedPrivate is a private bucket whose copy does not read back as
// what was written.
type tamperedPrivate struct {
	*media.PrivateStorage
}

func (p tamperedPrivate) Read(ctx context.Context, obj media.SealedObject) ([]byte, error) {
	data, err := p.PrivateStorage.Read(ctx, obj)
	return append(data, 'x'), err
}

// The private copy is read back before the Media points at it: one that
// does not hold the public object's bytes is deleted, and the Media stays a
// public legacy one.
func TestPostgresLegacyAnswersVerifyThePrivateCopy(t *testing.T) {
	d := newScanDatabase(t)
	public := &recordingPublic{MemoryBlob: d.blobs}
	answer := d.storedLegacyAnswer(t, pdfFile())
	storage := media.LegacyAnswerStorage{Public: public, Private: tamperedPrivate{media.NewPrivateStorage(d.private, transit.New(d.bao.Config()))}}

	report := d.runLegacyAnswers(t, storage, []media.LegacyAnswerPair{{MediaID: answer.ID, ResponseID: uuid.New()}}, media.LegacyAnswersMove, 25)

	if report.Failed != 1 || report.MovedNow != 0 {
		t.Fatalf("report %+v", report)
	}
	if got := d.get(t, answer.ID); got.Purpose != media.PurposeLegacy || got.Visibility != media.VisibilityPublic || got.Key != answer.Key {
		t.Fatalf("the Media changed: %+v", got)
	}
	if _, ok := d.private.Get(media.PrivateObjectKey(answer.Key)); ok {
		t.Fatal("the unverified private copy was kept")
	}
}

// One writing run at a time: the advisory lock is not taken twice, and is
// free again once released.
func TestPostgresLegacyAnswersLock(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	release, locked, err := d.store.LockLegacyAnswers(ctx)
	if err != nil || !locked {
		t.Fatalf("first lock: %v %v", locked, err)
	}
	if _, again, err := d.store.LockLegacyAnswers(ctx); err != nil || again {
		t.Fatalf("second lock while held: %v %v", again, err)
	}
	release()
	release2, locked, err := d.store.LockLegacyAnswers(ctx)
	if err != nil || !locked {
		t.Fatalf("lock after release: %v %v", locked, err)
	}
	release2()
}

// -limit bounds a run; the rest is left for the next. A public object whose
// delete fails stays recorded as not deleted, and the next finalize deletes
// it.
func TestPostgresLegacyAnswersResumeWhereTheyStopped(t *testing.T) {
	d := newScanDatabase(t)
	public := &recordingPublic{MemoryBlob: d.blobs}
	var pairs []media.LegacyAnswerPair
	for range 3 {
		pairs = append(pairs, media.LegacyAnswerPair{MediaID: d.storedLegacyAnswer(t, pdfFile()).ID, ResponseID: uuid.New()})
	}

	if first := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersMove, 2); first.MovedNow != 2 || first.Remaining != 1 {
		t.Fatalf("first move %+v", first)
	}
	if second := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersMove, 2); second.ToMove != 1 || second.MovedNow != 1 || second.AwaitingScan != 2 || second.Remaining != 0 {
		t.Fatalf("second move %+v", second)
	}
	if report := d.pass(t); report.Clean != 3 {
		t.Fatalf("scan %+v", report)
	}
	public.failDeletes = 1
	if first := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersFinalize, 2); first.FinalizedNow != 1 || first.Failed != 1 || first.Remaining != 1 {
		t.Fatalf("first finalize %+v", first)
	}
	if second := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersFinalize, 2); second.ReadyToFinalize != 2 || second.FinalizedNow != 2 || second.Failed+second.Remaining != 0 {
		t.Fatalf("second finalize %+v", second)
	}
	for _, pair := range pairs {
		got := d.get(t, pair.MediaID)
		if _, ok := d.blobs.Get(strings.TrimPrefix(got.Key, media.PrivateObjectKey(""))); ok || got.Visibility != media.VisibilityPrivate {
			t.Fatalf("%s: %s, public object still there %v", pair.MediaID, got.Visibility, ok)
		}
	}
}

// A Media used by another product or by core too, or archived, is never
// moved, even when Skyforms' export names it.
func TestPostgresLegacyAnswersLeaveSharedAndArchivedMediaAlone(t *testing.T) {
	d := newScanDatabase(t)
	ctx := context.Background()
	public := &recordingPublic{MemoryBlob: d.blobs}
	cms := d.storedLegacyAnswer(t, pdfFile())
	d.attachFor(t, cmsService, cms, media.Owner{Service: authz.ProductCMS, Type: "page", ID: uuid.NewString()}, media.RoleCMSFile)
	archived := d.storedLegacyAnswer(t, pdfFile())
	if err := d.store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	pairs := []media.LegacyAnswerPair{{MediaID: cms.ID, ResponseID: uuid.New()}, {MediaID: archived.ID, ResponseID: uuid.New()}}

	report := d.moveLegacyAnswers(t, public, pairs, media.LegacyAnswersMove, 25)

	if report.Shared != 1 || report.Archived != 1 || report.MovedNow+report.ToMove+report.Failed != 0 || len(public.deleted) != 0 {
		t.Fatalf("report %+v, deleted %v", report, public.deleted)
	}
	for _, m := range []media.Media{cms, archived} {
		if got := d.get(t, m.ID); got.Visibility != media.VisibilityPublic || got.Purpose != media.PurposeLegacy {
			t.Fatalf("%s changed: %s %s", m.ID, got.Purpose, got.Visibility)
		}
	}
}
