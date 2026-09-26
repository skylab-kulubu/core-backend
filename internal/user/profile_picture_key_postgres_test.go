package user_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A profile's picture is read from the Media it links, so an absolute
// address stored before (https://cdn…) no longer decides it: the caller
// builds the address from the configured base.
func TestPostgresProfilePictureIsReadFromItsMedia(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	users := user.NewService(store)
	subjectID := uuid.New()
	if _, _, err := users.Ensure(ctx, subjectID, user.Profile{
		Email: "key@example.com", FirstName: "Key", LastName: "Holder", Username: "key-holder",
	}); err != nil {
		t.Fatal(err)
	}
	picture, err := media.NewPostgresStore(pool).Create(ctx, media.Media{
		Name: "me.png", Type: "image/png", Kind: media.KindImage, Key: "images/me-key", UploadedBy: subjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// As an upload before this change stored it: the absolute address.
	if _, err := pool.Exec(ctx, `UPDATE users SET profile_picture_id = $2, profile_picture_url = 'https://cdn.yildizskylab.com/images/me-key' WHERE id = $1`, subjectID, picture.ID); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfilePictureURL != "images/me-key" {
		t.Fatalf("profile picture %q, want the Media's key", got.ProfilePictureURL)
	}
	updated, err := users.Patch(ctx, subjectID, user.ProfilePatch{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProfilePictureURL != "images/me-key" {
		t.Fatalf("after an update %q", updated.ProfilePictureURL)
	}
	if _, err := users.ClearProfilePicture(ctx, subjectID); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, subjectID); got.ProfilePictureURL != "" {
		t.Fatalf("cleared profile picture %q", got.ProfilePictureURL)
	}
}
