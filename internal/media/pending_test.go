package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func TestService_UploadForPurposeIsPendingUntilItsPendingTTL(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)

	before := time.Now()
	created, err := svc.UploadForPurpose(context.Background(), signedIn("60606060-6060-6060-6060-606060606060"), "profile_picture", uploaded("dot.png", "image/png", pngDot()))
	after := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), authz.Principal{}, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	// profile_picture keeps an unattached Media for 24 hours.
	if got.Status != media.StatusPending || got.ExpiresAt == nil ||
		got.ExpiresAt.Before(before.Add(24*time.Hour)) || got.ExpiresAt.After(after.Add(24*time.Hour)) {
		t.Fatalf("status %q expires %v, want pending until 24h after the upload", got.Status, got.ExpiresAt)
	}
}

func TestService_LegacyUploadIsPendingWithoutExpiry(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)

	created, err := svc.Upload(context.Background(), signedIn("61616161-6161-6161-6161-616161616161"), "dot.png", "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), authz.Principal{}, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != media.StatusPending || got.ExpiresAt != nil {
		t.Fatalf("status %q expires %v, want pending with no expiry", got.Status, got.ExpiresAt)
	}
}
