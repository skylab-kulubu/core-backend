package media_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func signedIn(id string) authz.Principal {
	return authz.Principal{ID: uuid.MustParse(id).String()}
}

func uploaded(name, contentType string, data []byte) media.UploadedFile {
	return media.UploadedFile{Name: name, ContentType: contentType, Data: data}
}

func TestService_UploadForPurposeRecordsThePurpose(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)

	created, err := svc.UploadForPurpose(context.Background(), signedIn("50505050-5050-5050-5050-505050505050"), "profile_picture", uploaded("dot.png", "image/png", pngDot()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if created.Purpose != "profile_picture" || got.Purpose != "profile_picture" || got.Kind != media.KindImage || got.Type != "image/png" {
		t.Fatalf("created %+v, stored %+v", created, got)
	}
	if meta, _ := blobs.Metadata(created.Key); meta != (media.BlobMetadata{ContentType: "image/png"}) {
		t.Fatalf("blob metadata %+v", meta)
	}
}

func TestService_UploadForUnknownPurposeIsRefused(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)

	_, err := svc.UploadForPurpose(context.Background(), signedIn("51515151-5151-5151-5151-515151515151"), "banner", uploaded("dot.png", "image/png", pngDot()))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrPurposeUnknown) || !errors.As(err, &refusal) || refusal.Purpose != "banner" {
		t.Fatalf("err = %v", err)
	}
}

func TestService_UploadForPurposeJudgesTheTypeByContent(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	p := signedIn("52525252-5252-5252-5252-525252525252")

	_, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("dot.png", "image/png", []byte("%PDF-1.7\n")))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrTypeNotAllowed) || !errors.As(err, &refusal) {
		t.Fatalf("PDF as a profile picture: err = %v", err)
	}
	wantAllowed := []string{"image/jpeg", "image/png", "image/webp", "image/gif"}
	if refusal.Purpose != "profile_picture" || !reflect.DeepEqual(refusal.AllowedTypes, wantAllowed) {
		t.Fatalf("refusal %+v", refusal)
	}

	created, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("cv.pdf", "application/pdf", pngDot()))
	if err != nil || created.Type != "image/png" {
		t.Fatalf("PNG named cv.pdf: created %+v err %v", created, err)
	}
}

// pngOfSize is a valid PNG padded after its end to exactly size bytes.
func pngOfSize(size int) []byte {
	out := make([]byte, size)
	copy(out, pngDot())
	return out
}

func TestService_UploadForPurposeKeepsItsMaximumSize(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	p := signedIn("53535353-5353-5353-5353-535353535353")

	_, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("big.png", "image/png", pngOfSize(5<<20+1)))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrTooLarge) || !errors.As(err, &refusal) || refusal.MaxBytes != 5<<20 || refusal.Purpose != "profile_picture" {
		t.Fatalf("5 MiB + 1 byte profile picture: err = %v", err)
	}
	if _, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("fits.png", "image/png", pngOfSize(5<<20))); err != nil {
		t.Fatalf("5 MiB profile picture: %v", err)
	}
}

func TestService_EventCoverIsUploadedByPeopleWhoMayCreateEvents(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	for _, tc := range []struct {
		who     string
		groups  []string
		allowed bool
	}{
		{"team member", []string{"/UYELER/ARGE/WEBLAB"}, false},
		{"team leader", []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}, true},
		{"member of a team whose members create Events", []string{"/UYELER/ORGANIZASYON/GECEKODU"}, true},
		{"privileged", []string{"/UYELER/YK"}, true},
	} {
		p := authz.Principal{ID: uuid.NewString(), Groups: tc.groups}
		_, err := svc.UploadForPurpose(context.Background(), p, "event_cover", uploaded("cover.png", "image/png", pngDot()))
		if tc.allowed && err != nil {
			t.Errorf("%s: %v", tc.who, err)
		}
		if !tc.allowed && !errors.Is(err, media.ErrPurposeForbidden) {
			t.Errorf("%s: err = %v, want %v", tc.who, err, media.ErrPurposeForbidden)
		}
	}
}

func TestService_PrivatePurposeIsRefusedWhilePrivateMediaIsOff(t *testing.T) {
	t.Parallel()
	blobs := &recordingBlobStore{}
	store := media.NewMemoryStore()
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")

	_, err := svc.UploadForPurpose(context.Background(), signedIn("54545454-5454-5454-5454-545454545454"), "answer_file", uploaded("cv.pdf", "application/pdf", []byte("%PDF-1.7\n")))
	if !errors.Is(err, media.ErrPrivateMediaDisabled) {
		t.Fatalf("answer file: err = %v, want %v", err, media.ErrPrivateMediaDisabled)
	}
	stored, _ := store.List(context.Background())
	if len(blobs.objects) != 0 || len(stored) != 0 {
		t.Fatalf("refused private upload left %d objects and %d records", len(blobs.objects), len(stored))
	}
}

func TestService_CertificateAssetIsUploadedByPeopleWhoMakeCertificateTemplates(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	for _, tc := range []struct {
		who    string
		p      authz.Principal
		refuse error
	}{
		{"team member", authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}, media.ErrPurposeForbidden},
		{"team member managing templates", authz.Principal{
			ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}, Roles: []string{"certificate:template:manage"},
		}, media.ErrPrivateMediaDisabled},
		{"team leader", authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, media.ErrPrivateMediaDisabled},
	} {
		_, err := svc.UploadForPurpose(context.Background(), tc.p, "certificate_asset", uploaded("background.png", "image/png", pngDot()))
		if !errors.Is(err, tc.refuse) {
			t.Errorf("%s: err = %v, want %v", tc.who, err, tc.refuse)
		}
	}
}

func TestService_DirectUploadPurposeIsRefusedThroughCore(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	organizer := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	for _, purpose := range []string{"video", "club_file"} {
		_, err := svc.UploadForPurpose(context.Background(), organizer, purpose, uploaded("material.pdf", "application/pdf", []byte("%PDF-1.7\n")))
		if !errors.Is(err, media.ErrDirectUploadOnly) {
			t.Errorf("%s: err = %v, want %v", purpose, err, media.ErrDirectUploadOnly)
		}
	}
}

func TestService_ServiceOnlyPurposeIsRefusedToEveryPerson(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	admin := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ADMIN"}}
	_, err := svc.UploadForPurpose(context.Background(), admin, "answer_file_large", uploaded("build.zip", "application/zip", []byte("PK\x03\x04")))
	if !errors.Is(err, media.ErrPurposeForbidden) {
		t.Fatalf("err = %v, want %v", err, media.ErrPurposeForbidden)
	}
}

func TestService_PurposePDFStartsWithItsHeader(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	p := signedIn("55555555-5555-5555-5555-000000000055")
	prefixed := []byte("<html><!-- -->\n%PDF-1.7\n")

	_, err := svc.UploadForPurpose(context.Background(), p, "cms_file", uploaded("bylaws.pdf", "application/pdf", prefixed))
	if !errors.Is(err, media.ErrTypeNotAllowed) {
		t.Fatalf("PDF marker after other bytes: err = %v, want %v", err, media.ErrTypeNotAllowed)
	}
	if _, err := svc.UploadForPurpose(context.Background(), p, "cms_file", uploaded("bylaws.pdf", "application/pdf", []byte("%PDF-1.7\n"))); err != nil {
		t.Fatalf("PDF with its header first: %v", err)
	}
	// Media uploaded without a purpose keep the old rule: the marker within
	// the first KiB.
	if _, err := svc.Upload(context.Background(), p, "old.pdf", "application/pdf", prefixed); err != nil {
		t.Fatalf("legacy PDF: %v", err)
	}
}

func TestService_LegacyPurposeCannotBeNamed(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)

	_, err := svc.UploadForPurpose(context.Background(), signedIn("56565656-5656-5656-5656-565656565656"), media.PurposeLegacy, uploaded("page.html", "text/html", []byte("<html></html>")))
	if !errors.Is(err, media.ErrPurposeUnknown) {
		t.Fatalf("err = %v, want %v", err, media.ErrPurposeUnknown)
	}
}
