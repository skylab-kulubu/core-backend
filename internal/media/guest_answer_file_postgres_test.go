package media_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// In the database, Skyforms' guest Answer file is staged and stored for no
// one (uploaded_by and the staging subject NULL: its service account has no
// core account), linked to a response for no one through the database's
// purpose check, and opened by the reviewer Skyforms names.
func TestPostgresGuestAnswerFileBelongsToNoOne(t *testing.T) {
	db := newPrivateDatabase(t)
	ctx := context.Background()

	guest, err := db.svc.UploadForPurpose(ctx, formsService, media.PurposeAnswerFileGuest, uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	var uploader *uuid.UUID
	var staged int
	if err := db.pool.QueryRow(ctx, `SELECT uploaded_by, (SELECT count(*) FROM media_upload_staging WHERE object_key = media.file_url)
		FROM media WHERE id = $1`, guest.ID).Scan(&uploader, &staged); err != nil {
		t.Fatal(err)
	}
	if uploader != nil || staged != 0 {
		t.Fatalf("uploaded_by %v, %d staging rows left", uploader, staged)
	}
	if got := db.get(t, guest.ID); got.UploadedBy != uuid.Nil || got.Purpose != media.PurposeAnswerFileGuest || got.Encryption == nil {
		t.Fatalf("stored %+v", got)
	}

	if _, created, err := db.svc.Attach(ctx, formsService, guest.ID, media.AttachRequest{
		Owner: media.Owner{Service: authz.ProductForms, Type: "response", ID: uuid.NewString()}, Role: media.RoleFormsAnswer,
	}); err != nil || !created {
		t.Fatalf("attach for no one: created %v, err %v", created, err)
	}
	attached(t, db.get(t, guest.ID))

	link, err := db.svc.IssueReadLink(ctx, formsService, guest.ID, forPerson(reviewer.String()))
	if err != nil {
		t.Fatal(err)
	}
	content, err := db.svc.OpenContent(ctx, guest.ID, tokenOf(t, link), "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, content); string(got) != string(pdfFile()) {
		t.Fatalf("opened %q", got)
	}
}

// Account erasure never touches a guest Answer file: it has no uploader, so
// no person's erasure records it, and it keeps its name and its object.
func TestPostgresAccountErasureLeavesGuestAnswerFilesAlone(t *testing.T) {
	p := newErasedPerson(t)
	ctx := context.Background()
	guest, err := p.svc.UploadForPurpose(ctx, formsService, media.PurposeAnswerFileGuest, uploaded("Ada_Organizer_CV.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	p.attachFor(t, formsService, guest, media.Owner{Service: authz.ProductForms, Type: "response", ID: uuid.NewString()}, media.RoleFormsAnswer)

	p.anonymize(t)
	for _, id := range p.recorded(t) {
		if id == guest.ID {
			t.Fatal("the person's erasure recorded the guest Answer file")
		}
	}
	p.eraseProfileMedia(t, p.buckets())

	got := p.get(t, guest.ID)
	if got.Name != "Ada_Organizer_CV.pdf" || got.BlobPurgedAt != nil || got.DeletedAt != nil || got.Status != media.StatusAttached {
		t.Fatalf("guest Answer file after the erasure: %+v", got)
	}
	if _, ok := p.private.Get(guest.Key); !ok {
		t.Fatal("the guest Answer file's object is gone")
	}
	if !p.gone(t, p.answer) {
		t.Fatal("the person's own Answer file was not purged")
	}
}
