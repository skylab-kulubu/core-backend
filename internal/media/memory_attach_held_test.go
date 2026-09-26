package media_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// The in-memory store gives a held Media back legacy only with a Media
// attachment it writes, as the Postgres store's transaction does: when the
// same link is already there, nothing changes.
func TestMemoryAttachHeldChangesNothingWhenTheLinkIsAlreadyThere(t *testing.T) {
	ctx := context.Background()
	store := media.NewMemoryStore()
	cover, err := store.Create(ctx, media.Media{Name: "cover.png", Type: "image/png", Key: "images/cover.png", Purpose: media.PurposeEventCover, DetachExpiryHeld: true})
	if err != nil {
		t.Fatal(err)
	}
	link := media.Attachment{MediaID: cover.ID, Owner: homePage, Role: media.RoleCMSImage}
	existing, _, err := store.Attach(ctx, link)
	if err != nil {
		t.Fatal(err)
	}

	got, created, demotedFrom, err := store.AttachHeld(ctx, link)

	if err != nil || created || demotedFrom != "" || got.ID != existing.ID {
		t.Fatalf("attach held %+v created %v demoted from %q err %v, want the existing link", got, created, demotedFrom, err)
	}
	if m, _ := store.Get(ctx, cover.ID); m.Purpose != media.PurposeEventCover {
		t.Fatalf("purpose %q, want event_cover kept", m.Purpose)
	}
}
