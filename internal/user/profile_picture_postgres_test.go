package user_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresClearProfilePictureUnlinksAndArchivesOwnUpload(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	users := user.NewService(store)
	mediaStore := media.NewPostgresStore(pool)
	blobs := media.NewMemoryBlob()
	mediaSvc := media.NewService(mediaStore, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "")
	subjectID := uuid.New()
	principal := authz.Principal{ID: subjectID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}

	if _, _, err := users.Ensure(ctx, subjectID, user.Profile{
		Email: "picture@example.com", FirstName: "Ada", LastName: "Lovelace", Username: "ada",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET phone = '+905551112233' WHERE id = $1`, subjectID); err != nil {
		t.Fatal(err)
	}

	released, err := users.ClearProfilePicture(ctx, subjectID)
	if err != nil || released != nil {
		t.Fatalf("clear without picture released=%v err=%v", released, err)
	}

	picture, err := mediaStore.Create(ctx, media.Media{
		Name: "me.png", Type: "image/png", Kind: media.KindImage, Key: "images/me", UploadedBy: subjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, picture.Key, []byte("png"), picture.Type); err != nil {
		t.Fatal(err)
	}
	if _, err := users.SetProfilePicture(ctx, subjectID, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}

	released, err = users.ClearProfilePicture(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if released == nil || *released != picture.ID {
		t.Fatalf("released %v want %s", released, picture.ID)
	}
	if err := mediaSvc.ArchiveOwn(ctx, principal, *released); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfilePictureID != nil || got.ProfilePictureURL != "" {
		t.Fatalf("shadow still linked %+v", got)
	}
	if got.Phone != "+905551112233" || got.FirstName != "Ada" || got.Username != "ada" {
		t.Fatalf("clear wiped neighbours %+v", got)
	}
	var deletedAt *time.Time
	var deletedBy *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT deleted_at, deleted_by FROM media WHERE id = $1`, picture.ID).Scan(&deletedAt, &deletedBy); err != nil {
		t.Fatal(err)
	}
	if deletedAt == nil || deletedBy == nil || *deletedBy != subjectID {
		t.Fatalf("media not archived by the person: deletedAt=%v deletedBy=%v", deletedAt, deletedBy)
	}
	if _, ok := blobs.Get(picture.Key); !ok {
		t.Fatal("archive removed blob before recovery window")
	}
	if _, err := mediaSvc.Get(ctx, picture.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("archived picture still current: %v", err)
	}

	// Repeating both steps is a no-op that still succeeds.
	if again, err := users.ClearProfilePicture(ctx, subjectID); err != nil || again != nil {
		t.Fatalf("repeated clear released=%v err=%v", again, err)
	}
	if err := mediaSvc.ArchiveOwn(ctx, principal, picture.ID); err != nil {
		t.Fatalf("repeated archive: %v", err)
	}
	var deletedAgain *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id = $1`, picture.ID).Scan(&deletedAgain); err != nil {
		t.Fatal(err)
	}
	if deletedAgain == nil || !deletedAgain.Equal(*deletedAt) {
		t.Fatalf("repeated archive moved deleted_at from %v to %v", deletedAt, deletedAgain)
	}

	// Archived media cannot be linked again; a fresh upload can.
	if _, err := users.SetProfilePicture(ctx, subjectID, picture.ID, picture.Key); err == nil {
		t.Fatal("relinking archived media must be rejected")
	}
	second, err := mediaStore.Create(ctx, media.Media{
		Name: "again.png", Type: "image/png", Kind: media.KindImage, Key: "images/again", UploadedBy: subjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.SetProfilePicture(ctx, subjectID, second.ID, second.Key); err != nil {
		t.Fatal(err)
	}

	// A blocked account keeps its link untouched.
	if _, err := store.RequestDeletion(ctx, subjectID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := users.ClearProfilePicture(ctx, subjectID); !errors.Is(err, user.ErrAccountBlocked) {
		t.Fatalf("clear while deletion pending error = %v, want ErrAccountBlocked", err)
	}
	var linked *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT profile_picture_id FROM users WHERE id = $1`, subjectID).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked == nil || *linked != second.ID {
		t.Fatalf("blocked account lost its picture link: %v", linked)
	}
}
