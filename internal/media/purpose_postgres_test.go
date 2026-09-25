package media_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresMediaKeepsItsPurpose(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{
		Email: "purpose-owner@example.com", FirstName: "Purpose", LastName: "Owner", Username: "purpose-owner",
	}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)

	cover, err := store.Create(ctx, media.Media{
		Name: "cover.png", Type: "image/png", Kind: media.KindImage, Key: "images/cover", UploadedBy: uploader, Purpose: "event_cover",
	})
	if err != nil {
		t.Fatal(err)
	}
	unnamed, err := store.Create(ctx, media.Media{
		Name: "notes.txt", Type: "text/plain", Kind: media.KindFile, Key: "files/notes", UploadedBy: uploader,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A row stored before Media purpose existed.
	stored := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind)
		VALUES ($1, 'old.pdf', 'application/pdf', 'files/old', 3, $2, 'FILE')`, stored, uploader); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[uuid.UUID]string{cover.ID: "event_cover", unnamed.ID: media.PurposeLegacy, stored: media.PurposeLegacy} {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Purpose != want {
			t.Errorf("media %s purpose %q, want %q", id, got.Purpose, want)
		}
	}
}
