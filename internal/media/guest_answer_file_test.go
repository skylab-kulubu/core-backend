package media_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
)

// A guest Answer file is an Answer file sent to a Skyforms form that takes
// answers without sign-in: no person uploads it, Skyforms' service account
// does, and it is private, encrypted and scanned like any Answer file.
func TestCatalogue_GuestAnswerFilesAreSkyformsOwnUploads(t *testing.T) {
	t.Parallel()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	guest, ok := catalogue.Lookup(media.PurposeAnswerFileGuest)
	if !ok {
		t.Fatal("no answer_file_guest purpose")
	}
	if guest.Uploader != authz.MediaUploaderServiceOnly || guest.Attach != media.AttachService || guest.OwningProduct() != authz.ProductForms {
		t.Errorf("uploaded by %q, attached by %q for %q", guest.Uploader, guest.Attach, guest.OwningProduct())
	}
	if guest.Visibility != media.VisibilityPrivate || !guest.Encrypted || !guest.Scan || guest.Transport != media.TransportSingleStep {
		t.Errorf("visibility %s, encrypted %v, scan %v, transport %s", guest.Visibility, guest.Encrypted, guest.Scan, guest.Transport)
	}
	if !slices.Equal(guest.Types, []string{"application/pdf", "image/jpeg", "image/png"}) || guest.MaxBytes != 10<<20 {
		t.Errorf("types %v, max %d bytes", guest.Types, guest.MaxBytes)
	}
	if !guest.Image.Reencode || guest.Image.MaxDimension != 2560 || guest.PendingTTL.Hours() != 24 {
		t.Errorf("image %+v, pending %s", guest.Image, guest.PendingTTL)
	}
}

// Skyforms' service account uploads a guest Answer file: it is stored
// encrypted in the private bucket, waits for its scan, and has no uploader,
// since no person (and no core account) sent it.
func TestService_SkyformsUploadsAGuestAnswerFileWithNoUploader(t *testing.T) {
	t.Parallel()
	pm, queue := newScannedMedia(t)
	ctx := context.Background()

	created, err := pm.svc.UploadForPurpose(ctx, formsService, media.PurposeAnswerFileGuest, uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	if created.UploadedBy != uuid.Nil {
		t.Fatalf("uploaded by %s, want no one", created.UploadedBy)
	}
	if created.Status != media.StatusScanning || created.Visibility != media.VisibilityPrivate || created.URL != "" || created.ExpiresAt == nil {
		t.Fatalf("created %+v", created)
	}
	if queue.n.Load() != 1 {
		t.Fatalf("the scan worker was nudged %d times", queue.n.Load())
	}
	if _, ok := pm.public.Get(created.Key); ok {
		t.Fatal("the guest Answer file reached the public bucket")
	}
	if stored, ok := pm.private.Get(created.Key); !ok || bytes.Contains(stored, pdfFile()) {
		t.Fatalf("private object present %v, holds the plaintext", ok)
	}
	// Skyforms reads its status (scanning, then pending or rejected).
	got, err := pm.svc.Get(ctx, formsService, created.ID)
	if err != nil || got.Status != media.StatusScanning || got.UploadedBy != uuid.Nil {
		t.Fatalf("Skyforms reads %+v, %v", got, err)
	}

	// The purpose's own rules hold for Skyforms as for anyone.
	if _, err := pm.svc.UploadForPurpose(ctx, formsService, media.PurposeAnswerFileGuest, uploaded("cv.docx", docxMIME, docxFile(t))); !errors.Is(err, media.ErrTypeNotAllowed) {
		t.Errorf("DOCX: err = %v, want %v", err, media.ErrTypeNotAllowed)
	}
	large := append(pdfFile(), make([]byte, 10<<20)...)
	if _, err := pm.svc.UploadForPurpose(ctx, formsService, media.PurposeAnswerFileGuest, uploaded("big.pdf", "application/pdf", large)); !errors.Is(err, media.ErrTooLarge) {
		t.Errorf("over 10 MiB: err = %v, want %v", err, media.ErrTooLarge)
	}
}

// Only Skyforms' own service account, with media:attach, uploads a guest
// Answer file: no person, whatever roles they hold, and no other product.
func TestService_GuestAnswerFileIsSkyformsServiceAccountsAlone(t *testing.T) {
	t.Parallel()
	pm, _ := newScannedMedia(t)
	withoutRole := formsService
	withoutRole.Roles = nil
	for name, p := range map[string]authz.Principal{
		"a member":                       signedIn("90909090-9090-9090-9090-909090909090"),
		"an admin with media:attach":     {ID: uuid.NewString(), Groups: []string{"/UYELER/ADMIN"}, Roles: []string{"media:attach"}},
		"the CMS service account":        cmsService,
		"Skyforms without media:attach":  withoutRole,
		"a service account of no client": {ID: uuid.NewString(), ServiceAccount: true, Roles: []string{"media:attach"}},
	} {
		_, err := pm.svc.UploadForPurpose(context.Background(), p, media.PurposeAnswerFileGuest, uploaded("cv.pdf", "application/pdf", pdfFile()))
		if !errors.Is(err, media.ErrPurposeForbidden) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrPurposeForbidden)
		}
	}
	if stored, _ := pm.store.List(context.Background()); len(stored) != 0 {
		t.Fatalf("refused uploads stored %d Media", len(stored))
	}
}

// Skyforms' service account is refused, nothing stored, until this side has
// private Media and a malware scanner: the purpose is off in production
// until both are configured.
func TestService_GuestAnswerFileNeedsPrivateMediaAndAScanner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	file := uploaded("cv.pdf", "application/pdf", pdfFile())

	store := media.NewMemoryStore()
	off := media.NewServiceWithOptions(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{ServiceProducts: []authz.Product{authz.ProductForms}})
	if _, err := off.UploadForPurpose(ctx, formsService, media.PurposeAnswerFileGuest, file); !errors.Is(err, media.ErrPrivateMediaDisabled) {
		t.Errorf("private Media off: err = %v, want %v", err, media.ErrPrivateMediaDisabled)
	}

	bao := transittest.NewServer(t)
	noScanner := media.NewServiceWithOptions(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: []authz.Product{authz.ProductForms},
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(media.NewMemoryBlob(), transit.New(bao.Config())),
				LinkKey: bytes.Repeat([]byte{7}, 32), LinkOrigin: "https://api.example.test/", AccessLog: store, Now: bao.Clock.Now,
			},
		})
	if _, err := noScanner.UploadForPurpose(ctx, formsService, media.PurposeAnswerFileGuest, file); !errors.Is(err, media.ErrPurposeNeedsScanner) {
		t.Errorf("no scanner: err = %v, want %v", err, media.ErrPurposeNeedsScanner)
	}
	if stored, _ := store.List(ctx); len(stored) != 0 {
		t.Fatalf("refused uploads stored %d Media", len(stored))
	}
}

// guestAnswerFile is a guest Answer file Skyforms uploaded, clean (the test
// catalogue scans nothing).
func (pm privateMedia) guestAnswerFile(t *testing.T) media.Media {
	t.Helper()
	created, err := pm.svc.UploadForPurpose(context.Background(), formsService, media.PurposeAnswerFileGuest, uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// A guest Answer file has no person: Skyforms links it to the response
// without onBehalfOf. A personal Answer file still needs its uploader as
// onBehalfOf, and without one is refused like a Media that does not exist.
func TestService_SkyformsAttachesAGuestAnswerFileForNoOne(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	ctx := context.Background()
	guest := pm.guestAnswerFile(t)
	response := media.Owner{Service: authz.ProductForms, Type: "response", ID: uuid.NewString()}

	link, created, err := pm.svc.Attach(ctx, formsService, guest.ID, media.AttachRequest{Owner: response, Role: media.RoleFormsAnswer})
	if err != nil || !created || link.MediaID != guest.ID {
		t.Fatalf("guest Answer file: link %+v, created %v, err %v", link, created, err)
	}
	if got, _ := pm.store.Get(ctx, guest.ID); got.Status != media.StatusAttached || got.ExpiresAt != nil {
		t.Fatalf("attached guest Answer file: %s, expires %v", got.Status, got.ExpiresAt)
	}

	personal := pm.answerFile(t, signedIn("91919191-9191-9191-9191-919191919191"))
	_, _, err = pm.svc.Attach(ctx, formsService, personal.ID, media.AttachRequest{Owner: response, Role: media.RoleFormsAnswer})
	requireNotLinkable(t, err, personal, media.RoleFormsAnswer, "a personal Answer file for no one")
}

// The reviewer Skyforms decided may open a guest Answer file gets a read
// link to it, as for any Answer file.
func TestService_ReviewerOpensAGuestAnswerFile(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	ctx := context.Background()
	guest := pm.guestAnswerFile(t)

	link, err := pm.svc.IssueReadLink(ctx, formsService, guest.ID, forPerson(reviewer.String()))
	if err != nil {
		t.Fatal(err)
	}
	content, err := pm.svc.OpenContent(ctx, guest.ID, tokenOf(t, link), "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, content); !bytes.Equal(got, pdfFile()) {
		t.Fatalf("opened %q", got)
	}
	if log := pm.store.ReadLinkLog(guest.ID); len(log) != 1 || log[0].OnBehalfOf != reviewer || len(log[0].Opens) != 1 {
		t.Fatalf("access log %+v", log)
	}
	if _, err := pm.svc.IssueReadLink(ctx, cmsService, guest.ID, forPerson(reviewer.String())); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("the CMS asks for a link: err = %v, want %v", err, media.ErrNotFound)
	}
}
