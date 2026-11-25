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

func (d *scanDatabase) moveLegacyAnswers(t *testing.T, public *recordingPublic, pairs []media.LegacyAnswerPair, apply bool, limit int) media.LegacyAnswerReport {
	t.Helper()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	report, err := media.MoveLegacyAnswers(context.Background(), d.store, d.legacyAnswerStorage(public), catalogue, pairs, apply, limit,
		func() time.Time { return d.now }, func(err error) { t.Logf("move: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return report
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

// The dry run only counts, by what each Media is; the move then takes the
// legacy Answer file alone: encrypted in the private bucket at
// private/<its key>, an answer_file waiting for its scan with no expiry,
// linked to both of its responses for Skyforms, its public object deleted
// through the public bucket (which queues the CDN purge), and the move
// recorded. A second run finds it moved and changes nothing.
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

	dry := d.moveLegacyAnswers(t, public, pairs, false, 25)

	if dry.Pairs != 6 || dry.Media != 4 || dry.ToMove != 1 || dry.ToMoveBytes != answer.Size || dry.Shared != 1 || dry.NotLegacy != 1 || dry.NotFound != 1 ||
		dry.MovedNow+dry.PublicDeletedNow+dry.Failed+dry.Remaining != 0 {
		t.Fatalf("dry run %+v", dry)
	}
	if got := d.get(t, answer.ID); got.Purpose != media.PurposeLegacy || got.Key != answer.Key || len(public.deleted) != 0 {
		t.Fatalf("the dry run changed the Media (%s at %s) or deleted %v", got.Purpose, got.Key, public.deleted)
	}

	applied := d.moveLegacyAnswers(t, public, pairs, true, 25)

	if applied.ToMove != 1 || applied.MovedNow != 1 || applied.PublicDeletedNow != 1 || applied.Failed+applied.Changed+applied.Remaining != 0 {
		t.Fatalf("apply %+v", applied)
	}
	got := d.get(t, answer.ID)
	if got.Purpose != media.PurposeAnswerFile || got.Visibility != media.VisibilityPrivate || got.Encryption == nil ||
		got.Key != media.PrivateObjectKey(answer.Key) || got.Status != media.StatusScanning || got.ExpiresAt != nil ||
		got.Name != answer.Name || got.UploadedBy != answer.UploadedBy || !got.CreatedAt.Equal(answer.CreatedAt) {
		t.Fatalf("moved %+v", got)
	}
	if _, ok := d.blobs.Get(answer.Key); ok {
		t.Fatal("the public object is still there")
	}
	if !slices.Contains(public.deleted, answer.Key) {
		t.Fatalf("deleted %v, want the public key through the public bucket", public.deleted)
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
	var publicKey string
	var deleted bool
	if err := d.pool.QueryRow(ctx, `SELECT public_key, public_deleted_at IS NOT NULL FROM media_legacy_answer_moves WHERE media_id = $1`, answer.ID).
		Scan(&publicKey, &deleted); err != nil || publicKey != answer.Key || !deleted {
		t.Fatalf("move record %q deleted %v (%v)", publicKey, deleted, err)
	}
	if c := d.get(t, cover.ID); c.Visibility != media.VisibilityPublic || c.Key != cover.Key {
		t.Fatalf("the Event cover was moved: %+v", c)
	}

	again := d.moveLegacyAnswers(t, public, pairs, true, 25)
	if again.Moved != 1 || again.ToMove+again.MovedNow+again.PublicDeletedNow+again.Failed != 0 {
		t.Fatalf("second run %+v", again)
	}
}

// A moved Answer file is scanned by the scan worker like any other, its
// deadline counted from the move: one uploaded months ago is not rejected
// as scan_timeout. Clean, it is attached to its responses and Skyforms gets
// a read link for its reviewer; infected, it is rejected and its private
// object deleted.
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
	if report := d.moveLegacyAnswers(t, public, pairs, true, 25); report.MovedNow != 2 {
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
}

// -limit bounds a run; the rest is left for the next. A public object whose
// delete fails is left recorded as moved, and the next run deletes it.
func TestPostgresLegacyAnswersResumeWhereTheyStopped(t *testing.T) {
	d := newScanDatabase(t)
	public := &recordingPublic{MemoryBlob: d.blobs}
	var pairs []media.LegacyAnswerPair
	for range 3 {
		pairs = append(pairs, media.LegacyAnswerPair{MediaID: d.storedLegacyAnswer(t, pdfFile()).ID, ResponseID: uuid.New()})
	}
	public.failDeletes = 1

	first := d.moveLegacyAnswers(t, public, pairs, true, 2)

	if first.MovedNow != 2 || first.PublicDeletedNow != 1 || first.Failed != 1 || first.Remaining != 1 {
		t.Fatalf("first run %+v", first)
	}

	second := d.moveLegacyAnswers(t, public, pairs, true, 2)

	if second.PublicDeletePending != 1 || second.ToMove != 1 || second.MovedNow != 1 || second.PublicDeletedNow != 2 || second.Failed+second.Remaining != 0 {
		t.Fatalf("second run %+v", second)
	}
	for _, pair := range pairs {
		got := d.get(t, pair.MediaID)
		if got.Visibility != media.VisibilityPrivate {
			t.Fatalf("%s left %s", pair.MediaID, got.Visibility)
		}
		if _, ok := d.blobs.Get(strings.TrimPrefix(got.Key, media.PrivateObjectKey(""))); ok {
			t.Fatalf("%s: public object still there", pair.MediaID)
		}
	}
	if third := d.moveLegacyAnswers(t, public, pairs, true, 2); third.Moved != 3 || third.MovedNow+third.PublicDeletedNow+third.Failed != 0 {
		t.Fatalf("third run %+v", third)
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

	report := d.moveLegacyAnswers(t, public, pairs, true, 25)

	if report.Shared != 1 || report.Archived != 1 || report.MovedNow+report.ToMove+report.Failed != 0 || len(public.deleted) != 0 {
		t.Fatalf("report %+v, deleted %v", report, public.deleted)
	}
	for _, m := range []media.Media{cms, archived} {
		if got := d.get(t, m.ID); got.Visibility != media.VisibilityPublic || got.Purpose != media.PurposeLegacy {
			t.Fatalf("%s changed: %s %s", m.ID, got.Purpose, got.Visibility)
		}
	}
}
