package media_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A record reads the Media it links with media.LinkedImageSQL; only a Media
// served from the CDN gets addresses from it. A private Media, or one whose
// object is purged, gets none, whatever it records.
func TestPostgresLinkedImageHasAddressesOnlyWhenServedPublicly(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{Email: "linked@example.test"}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	sizes := map[string]media.SizeObject{
		media.SizeCard: {ImageSize: media.ImageSize{Width: 400, Height: 300}, Type: "image/jpeg"},
		media.SizePage: {ImageSize: media.ImageSize{Width: 1200, Height: 900}, Type: "image/jpeg"},
	}
	create := func(m media.Media) media.Media {
		t.Helper()
		m.Name, m.Type, m.Kind, m.UploadedBy, m.Width, m.Height = "photo.jpg", "image/jpeg", media.KindImage, uploader, 1600, 1200
		created, err := store.Create(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	public := create(media.Media{Key: "images/public", Purpose: media.PurposeEventCover, SizeObjects: sizes})
	private := create(media.Media{
		Key: "private/images/secret", Purpose: media.PurposeCertificateAsset, SizeObjects: sizes, Visibility: media.VisibilityPrivate,
		Encryption: &media.Encryption{Algorithm: "aes-256-gcm-chunked-v1", WrappedKey: "vault:v1:AAAA", KeyVersion: 1},
	})
	purged := create(media.Media{Key: "images/purged", Purpose: media.PurposeEventCover, SizeObjects: sizes})
	if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at = now(), blob_purge_started_at = now() WHERE id = $1`, purged.ID); err != nil {
		t.Fatal(err)
	}

	read := func(id uuid.UUID) *media.LinkedImage {
		t.Helper()
		var image *media.LinkedImage
		if err := pool.QueryRow(ctx, `SELECT `+media.LinkedImageSQL("m")+` FROM media m WHERE m.id = $1`, id).Scan(&image); err != nil {
			t.Fatal(err)
		}
		return image
	}
	addresses := media.Addresses{Base: "https://cdn.example.test"}

	want := map[string]media.ImageAddress{
		media.SizeCard: {URL: "https://cdn.example.test/images/public/card.jpg", Width: 400, Height: 300},
		media.SizePage: {URL: "https://cdn.example.test/images/public/page.jpg", Width: 1200, Height: 900},
	}
	if got := addresses.LinkedSizes(read(public.ID)); !reflect.DeepEqual(got, want) {
		t.Fatalf("public Media sizes %+v, want %+v", got, want)
	}
	if got := addresses.LinkedSizes(read(private.ID)); got != nil {
		t.Fatalf("a private Media got addresses %+v", got)
	}
	if got := addresses.LinkedSizes(read(purged.ID)); got != nil {
		t.Fatalf("a purged Media got addresses %+v", got)
	}

	// A record that links no Media reads NULL: no image, no addresses.
	var none *media.LinkedImage
	if err := pool.QueryRow(ctx, `SELECT `+media.LinkedImageSQL("m")+` FROM (SELECT NULL::uuid AS id) x LEFT JOIN media m ON m.id = x.id`).Scan(&none); err != nil {
		t.Fatal(err)
	}
	if none != nil || addresses.LinkedSizes(none) != nil {
		t.Fatalf("no Media read as %+v", none)
	}
}
