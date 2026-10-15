package media_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func admin() authz.Principal {
	return authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
}

// answerFile uploads an Answer file for the respondent.
func (pm privateMedia) answerFile(t *testing.T, respondent authz.Principal) media.Media {
	t.Helper()
	created, err := pm.svc.UploadForPurpose(context.Background(), respondent, "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestService_PrivateMediaMetadataIsHiddenFromAnonymousAndMembers(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	respondent := signedIn("70707070-7070-7070-7070-707070707070")
	file := pm.answerFile(t, respondent)

	for name, p := range map[string]authz.Principal{
		"anonymous":       {},
		"a member":        signedIn("71717171-7171-7171-7171-717171717171"),
		"the CMS service": cmsService,
	} {
		if _, err := pm.svc.Get(context.Background(), p, file.ID); !errors.Is(err, media.ErrNotFound) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrNotFound)
		}
	}
}

// The uploader follows their own upload until a record holds it: they see
// its status and scan result (what the upload answered, nothing more), never
// an address. Once Skyforms attaches it, Skyforms decides, and the uploader
// gets 404 like anyone else.
func TestService_UploaderReadsTheirUnattachedPrivateMetadataWithoutAnAddress(t *testing.T) {
	t.Parallel()
	pm, _ := newScannedMedia(t)
	ctx := context.Background()
	uploader := "74747474-7474-7474-7474-747474747474"
	respondent := signedIn(uploader)
	file := pm.answerFile(t, respondent)

	got, err := pm.svc.Get(ctx, respondent, file.ID)
	if err != nil || got.ID != file.ID || got.Status != media.StatusScanning || got.URL != "" || got.Sizes != nil {
		t.Fatalf("uploader read while scanning: %+v, %v", got, err)
	}
	for _, scanned := range []struct {
		status media.Status
		result media.ScanResult
	}{{media.StatusPending, media.ScanClean}, {media.StatusRejected, media.ScanInfected}} {
		pm.store.SetScanned(file.ID, scanned.status, scanned.result)
		got, err := pm.svc.Get(ctx, respondent, file.ID)
		if err != nil || got.Status != scanned.status || got.ScanResult != scanned.result || got.URL != "" {
			t.Fatalf("uploader read when %s: %+v, %v", scanned.status, got, err)
		}
	}

	pm.store.SetScanned(file.ID, media.StatusPending, media.ScanClean)
	if _, _, err := pm.svc.Attach(ctx, formsService, file.ID, media.AttachRequest{
		Owner: media.Owner{Service: authz.ProductForms, Type: "response", ID: "r1"}, Role: media.RoleFormsAnswer, OnBehalfOf: uuid.MustParse(uploader),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pm.svc.Get(ctx, respondent, file.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("uploader read once attached: err = %v, want %v", err, media.ErrNotFound)
	}
}

func TestService_OwningProductAndAdminsReadPrivateMetadataWithoutAnAddress(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	file := pm.answerFile(t, signedIn("72727272-7272-7272-7272-727272727272"))

	for name, p := range map[string]authz.Principal{"Skyforms": formsService, "an admin": admin()} {
		got, err := pm.svc.Get(context.Background(), p, file.ID)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got.ID != file.ID || got.URL != "" || got.Visibility != media.VisibilityPrivate {
			t.Errorf("%s: got %+v", name, got)
		}
	}
	listed, err := pm.svc.List(context.Background(), admin())
	if err != nil || len(listed) != 1 || listed[0].URL != "" {
		t.Fatalf("admin list %+v, %v", listed, err)
	}
}

func TestService_PublicMediaMetadataStaysOpenToAnyone(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	picture, err := pm.svc.UploadForPurpose(context.Background(), signedIn("73737373-7373-7373-7373-737373737373"), "profile_picture", uploaded("me.png", "image/png", pngDot()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := pm.svc.Get(context.Background(), authz.Principal{}, picture.ID)
	if err != nil || got.URL == "" {
		t.Fatalf("anonymous read of a public Media: %+v, %v", got, err)
	}
}
