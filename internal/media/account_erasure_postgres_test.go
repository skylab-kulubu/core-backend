package media_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// erasedPerson is a person with one upload of each kind account erasure
// tells apart (media redesign ticket 07), in a database with private Media
// on, and their deletion request.
type erasedPerson struct {
	privateDatabase
	users   *user.PostgresStore
	person  uuid.UUID
	request user.DeletionRequest
	// oldPicture was their profile picture before picture, the current one.
	oldPicture, picture media.Media
	// answer is an Answer file, private, that a Skyforms response holds.
	answer media.Media
	// cover is an Event's cover image they uploaded; poster an SVG cover no
	// Event uses yet, stored to download under its name.
	cover, poster media.Media
	// legacyPDF is a legacy upload nothing uses; legacyImage is a legacy
	// upload a CMS page uses.
	legacyPDF, legacyImage media.Media
}

func newErasedPerson(t *testing.T) erasedPerson {
	t.Helper()
	db := newPrivateDatabase(t)
	ctx := context.Background()
	users := user.NewPostgresStore(db.pool)
	person := db.uploader()
	p := erasedPerson{privateDatabase: db, users: users, person: person}

	p.oldPicture = db.upload(t, media.PurposeProfilePicture)
	p.picture = db.upload(t, media.PurposeProfilePicture)
	for _, picture := range []media.Media{p.oldPicture, p.picture} {
		if _, err := user.NewService(users).SetProfilePicture(ctx, person, picture.ID, picture.Key); err != nil {
			t.Fatal(err)
		}
	}
	answer, err := db.svc.UploadForPurpose(ctx, db.organizer, media.PurposeAnswerFile, uploaded("Ada_Organizer_CV.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	p.answer = answer
	db.attachFor(t, formsService, p.answer, media.Owner{Service: authz.ProductForms, Type: "response", ID: uuid.NewString()}, media.RoleFormsAnswer)
	p.cover = db.upload(t, media.PurposeEventCover)
	if _, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &p.cover.ID}); err != nil {
		t.Fatal(err)
	}
	poster, err := db.svc.UploadForPurpose(ctx, db.organizer, media.PurposeEventCover, uploaded("Ada_Organizer_poster.svg", "image/svg+xml",
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`)))
	if err != nil {
		t.Fatal(err)
	}
	p.poster = poster
	if meta, _ := db.blobs.Metadata(poster.Key); !strings.Contains(meta.ContentDisposition, "Ada_Organizer_poster") {
		t.Fatalf("the poster is stored with disposition %q, want its name", meta.ContentDisposition)
	}
	legacyPDF, err := db.svc.Upload(ctx, db.organizer, "Ada_Organizer_transcript.pdf", "application/pdf", pdfFile())
	if err != nil {
		t.Fatal(err)
	}
	p.legacyPDF = legacyPDF
	p.legacyImage = db.withBlob(t, media.PurposeLegacy)
	db.attachFor(t, cmsService, p.legacyImage, homePage, media.RoleCMSImage)

	for _, item := range []media.Media{p.legacyPDF, p.legacyImage} {
		if got := db.get(t, item.ID); got.Purpose != media.PurposeLegacy {
			t.Fatalf("%s stored as %q, want legacy", item.Name, got.Purpose)
		}
	}
	request, err := users.RequestDeletion(ctx, person, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.request = request
	return p
}

func (p erasedPerson) anonymize(t *testing.T) {
	t.Helper()
	if err := p.users.AnonymizeAccount(context.Background(), p.person, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
}

// recorded is what anonymize_core recorded for erase_profile_media to purge.
func (p erasedPerson) recorded(t *testing.T) []uuid.UUID {
	t.Helper()
	rows, err := p.pool.Query(context.Background(), `SELECT media_id FROM account_deletion_media WHERE request_id = $1 ORDER BY media_id`, p.request.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func sortedIDs(items ...media.Media) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return ids
}

// anonymize_core records, by id, every upload of the person but their
// current profile picture, which the profile-picture erasure has
// (profile_media_id); it keeps no uploader and no file name on any of them.
func TestPostgresAnonymizeRecordsEveryUploadButTheCurrentPicture(t *testing.T) {
	p := newErasedPerson(t)
	p.anonymize(t)

	if got, want := p.recorded(t), sortedIDs(p.oldPicture, p.answer, p.cover, p.poster, p.legacyPDF, p.legacyImage); !slices.Equal(got, want) {
		t.Fatalf("recorded %v, want %v (every upload but the current picture)", got, want)
	}
	ids, err := p.users.MediaForDeletion(context.Background(), p.request.ID)
	if err != nil || len(ids) != 7 || ids[0] != p.picture.ID {
		t.Fatalf("media for deletion %v err %v, want the current picture, then the six records", ids, err)
	}
	for _, item := range []media.Media{p.oldPicture, p.picture, p.answer, p.cover, p.poster, p.legacyPDF, p.legacyImage} {
		got := p.get(t, item.ID)
		if got.UploadedBy != uuid.Nil || got.Name != "" {
			t.Errorf("%s kept uploader %v and name %q", item.Name, got.UploadedBy, got.Name)
		}
	}
}

// anonymize_core runs again when a crash lost its checkpoint. The person's
// uploads have no uploader by then, so a rerun finds none of them: it
// neither removes nor rewrites what the first run recorded, even when it goes
// through the whole anonymization again.
func TestPostgresAnonymizeAgainKeepsTheFirstRecords(t *testing.T) {
	p := newErasedPerson(t)
	p.anonymize(t)
	first := p.recorded(t)

	p.anonymize(t)
	if _, err := p.pool.Exec(context.Background(), `UPDATE users SET account_state = 'deletion_pending' WHERE id = $1`, p.person); err != nil {
		t.Fatal(err)
	}
	p.anonymize(t)

	if got := p.recorded(t); len(first) != 6 || !slices.Equal(got, first) {
		t.Fatalf("recorded %v after the reruns, %v after the first run", got, first)
	}
}

// buckets is the public and the private bucket as the erasure worker has
// them.
func (p erasedPerson) buckets() media.Buckets {
	return media.Buckets{Public: p.blobs, Private: media.NewPrivateStorage(p.private, transit.New(p.bao.Config()))}
}

// eraseProfileMedia does what the erasure worker's erase_profile_media step
// does: it erases each Media the request still names.
func (p erasedPerson) eraseProfileMedia(t *testing.T, blobs media.BlobStore) {
	t.Helper()
	ctx := context.Background()
	ids, err := p.users.MediaForDeletion(ctx, p.request.ID)
	if err != nil {
		t.Fatal(err)
	}
	eraser := media.NewImmediateBlobEraser(p.store, blobs)
	for _, id := range ids {
		if err := eraser.EnsureErased(ctx, id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
}

// kept reports whether the Media and its object are still there, with no
// uploader and no name left on it.
func (p erasedPerson) kept(t *testing.T, item media.Media) bool {
	t.Helper()
	got := p.get(t, item.ID)
	if got.UploadedBy != uuid.Nil || got.Name != "" {
		t.Fatalf("%s kept uploader %v and name %q", item.Key, got.UploadedBy, got.Name)
	}
	_, public := p.blobs.Get(item.Key)
	_, private := p.private.Get(item.Key)
	return got.BlobPurgedAt == nil && got.DeletedAt == nil && (public || private)
}

// gone reports whether the Media's object is purged from both buckets.
func (p erasedPerson) gone(t *testing.T, item media.Media) bool {
	t.Helper()
	_, public := p.blobs.Get(item.Key)
	_, private := p.private.Get(item.Key)
	return p.get(t, item.ID).BlobPurgedAt != nil && !public && !private
}

// Account erasure purges the person's own files at once and keeps the club
// content they uploaded, anonymous: personal purposes and the legacy uploads
// nothing uses go, the Event's cover and the legacy image a CMS page uses
// stay. The Answer file goes from the private bucket.
func TestPostgresAccountErasurePurgesPersonalMediaAndKeepsClubMedia(t *testing.T) {
	p := newErasedPerson(t)
	if _, ok := p.private.Get(p.answer.Key); !ok {
		t.Fatal("the Answer file is not in the private bucket")
	}
	p.anonymize(t)
	p.eraseProfileMedia(t, p.buckets())

	for _, item := range []media.Media{p.oldPicture, p.picture, p.answer, p.legacyPDF} {
		if !p.gone(t, item) {
			t.Errorf("personal Media %s was not purged", item.Key)
		}
	}
	for _, item := range []media.Media{p.cover, p.poster, p.legacyImage} {
		if !p.kept(t, item) {
			t.Errorf("club Media %s was not kept", item.Key)
		}
	}
	// The kept SVG downloads under its key now, not under the person's file
	// name, and the key never held any of the name.
	if meta, _ := p.blobs.Metadata(p.poster.Key); meta.ContentDisposition != "attachment" || meta.ContentType != "image/svg+xml" {
		t.Fatalf("the kept poster is served as %q, %q; want image/svg+xml, attachment", meta.ContentType, meta.ContentDisposition)
	}
	for _, part := range []string{"Ada", "Organizer", "poster"} {
		if strings.Contains(p.poster.Key, part) {
			t.Fatalf("the kept poster's key %q holds %q of its name", p.poster.Key, part)
		}
	}
	if got := p.recorded(t); len(got) != 0 {
		t.Fatalf("records %v left after their Media were handled", got)
	}
}

// Both steps run again after a crash: anonymize_core records nothing new,
// and erase_profile_media finds the purged Media purged and the club Media
// kept, and succeeds.
func TestPostgresAccountErasureRunsAgainWithoutHarm(t *testing.T) {
	p := newErasedPerson(t)
	p.anonymize(t)
	p.anonymize(t)
	p.eraseProfileMedia(t, p.buckets())
	p.eraseProfileMedia(t, p.buckets())

	for _, item := range []media.Media{p.oldPicture, p.picture, p.answer, p.legacyPDF} {
		if !p.gone(t, item) {
			t.Errorf("personal Media %s was not purged", item.Key)
		}
	}
	for _, item := range []media.Media{p.cover, p.poster, p.legacyImage} {
		if !p.kept(t, item) {
			t.Errorf("club Media %s was not kept", item.Key)
		}
	}
	if got := p.recorded(t); len(got) != 0 {
		t.Fatalf("records %v left", got)
	}
}

// An Answer file a Skyforms response still holds is purged all the same:
// personal purposes win. Its Media attachment stays, pointing at a purged
// Media, and Skyforms' reads of it are refused as not found (404), both a new
// read link and one issued before the erasure.
func TestPostgresAccountErasurePurgesAnAnswerFileAResponseHolds(t *testing.T) {
	p := newErasedPerson(t)
	ctx := context.Background()
	earlier, err := p.svc.IssueReadLink(ctx, formsService, p.answer.ID, forPerson(reviewer.String()))
	if err != nil {
		t.Fatal(err)
	}
	p.anonymize(t)
	p.eraseProfileMedia(t, p.buckets())

	if !p.gone(t, p.answer) {
		t.Fatal("the Answer file a response holds was not purged")
	}
	if got := p.attachmentsOf(t, p.answer.ID); len(got) != 1 {
		t.Fatalf("the Answer file's Media attachments %v, want the response's", got)
	}
	if _, err := p.svc.IssueReadLink(ctx, formsService, p.answer.ID, forPerson(reviewer.String())); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("read link to the purged Answer file: %v, want not found", err)
	}
	if _, err := p.svc.OpenContent(ctx, p.answer.ID, tokenOf(t, earlier), "203.0.113.9"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("opening an earlier read link to the purged Answer file: %v, want not found", err)
	}
}

// eraseDuringWrite runs an account erasure the first time metadata is
// written, before the write lands: the serving-policy backfill read the
// Media before the erasure and writes after it.
type eraseDuringWrite struct {
	*media.MemoryBlob
	erase func()
	once  sync.Once
}

func (b *eraseDuringWrite) SetMetadata(ctx context.Context, key string, meta media.BlobMetadata) error {
	b.once.Do(b.erase)
	return b.MemoryBlob.SetMetadata(ctx, key, meta)
}

// The serving-policy backfill that read an upload's file name before its
// uploader's erasure does not bring that name back after the erasure
// stripped it: it reads the Media again after writing and writes what it
// holds now.
func TestPostgresServingPolicyBackfillDoesNotBringBackAnErasedName(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	users := user.NewPostgresStore(db.pool)
	poster, err := db.store.Create(ctx, media.Media{
		Name: "Ada_Organizer_poster.svg", Type: "image/svg+xml", Kind: media.KindImage, Key: "images/" + uuid.NewString() + ".svg",
		UploadedBy: db.uploader(), Purpose: media.PurposeEventCover,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.blobs.Put(ctx, poster.Key, []byte("<svg/>"), media.BlobMetadata{ContentType: poster.Type}); err != nil {
		t.Fatal(err)
	}
	request, err := users.RequestDeletion(ctx, db.uploader(), nil)
	if err != nil {
		t.Fatal(err)
	}
	blobs := &eraseDuringWrite{MemoryBlob: db.blobs, erase: func() {
		at := time.Now().UTC()
		if err := users.AnonymizeAccount(ctx, db.uploader(), at, nil); err != nil {
			t.Error(err)
			return
		}
		ids, err := users.MediaForDeletion(ctx, request.ID)
		if err != nil {
			t.Error(err)
			return
		}
		for _, id := range ids {
			if err := media.NewImmediateBlobEraser(db.store, db.blobs).EnsureErased(ctx, id, at); err != nil {
				t.Error(err)
			}
		}
	}}

	if _, err := media.BackfillServingPolicy(ctx, db.store, blobs, func(err error) { t.Error(err) }); err != nil {
		t.Fatal(err)
	}
	if meta, _ := db.blobs.Metadata(poster.Key); meta.ContentDisposition != "attachment" {
		t.Fatalf("the backfill wrote the erased name back: disposition %q", meta.ContentDisposition)
	}
}
