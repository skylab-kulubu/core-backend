package certificate_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestTemplateDraftRefusesMediaOfAnotherPurpose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	store := certificate.NewMemoryStore()
	mediaStore := media.NewMemoryStore()
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), az, nil, nil,
		certificate.Options{Templates: store, Media: media.NewLinker(mediaStore)})
	admin := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	uploads := media.NewService(mediaStore, media.NewMemoryBlob(), az, "")
	cover, err := uploads.UploadForPurpose(ctx, admin, "event_cover", media.UploadedFile{Name: "cover.png", Data: pngDot()})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := uploads.Upload(ctx, admin, "background.png", "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}
	draft := func(background uuid.UUID) certificate.TemplateDraft {
		layout := versionedLayout("MEDIA")
		layout.BackgroundMediaID = &background
		return certificate.TemplateDraft{Name: "WEBLAB", OwnerTeam: "WEBLAB", SourceKind: "upload", Layout: layout}
	}

	_, err = service.CreateTemplate(ctx, admin, draft(cover.ID))
	var refusal *media.LinkRefusal
	if !errors.Is(err, media.ErrPurposeMismatch) || !errors.As(err, &refusal) || refusal.MediaID != cover.ID || refusal.Role != media.RoleCertificateAsset {
		t.Fatalf("event cover as certificate background: err = %v", err)
	}

	// Transition rule: a legacy Media is still accepted.
	created, err := service.CreateTemplate(ctx, admin, draft(legacy.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateTemplate(ctx, admin, created.ID, draft(cover.ID)); !errors.Is(err, media.ErrPurposeMismatch) {
		t.Fatalf("event cover on an update: err = %v", err)
	}
}

func pngDot() []byte {
	b, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		panic(err)
	}
	return b
}
