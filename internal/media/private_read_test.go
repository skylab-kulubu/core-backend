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
		"the uploader":    respondent,
		"the CMS service": cmsService,
	} {
		if _, err := pm.svc.Get(context.Background(), p, file.ID); !errors.Is(err, media.ErrNotFound) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrNotFound)
		}
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
