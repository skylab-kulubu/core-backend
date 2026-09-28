package media_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// Whether a Media can be served from the CDN is one rule, in Go
// (Media.Servable: the Media JSON's address, an image's sizes) and in SQL
// (media.ServableSQL: the records that list Media, such as an Event's files
// and videos, and their counts). The two agree on every state: public and
// current is servable; private, waiting for or rejected by its malware scan,
// or with its object being or already purged is not.
func TestPostgresServableSQLIsMediaServable(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	stored := func(m media.Media, afterwards string) media.Media {
		t.Helper()
		m.Name, m.Type, m.Kind, m.UploadedBy, m.Purpose = "x.pdf", "application/pdf", media.KindFile, db.uploader(), media.PurposeClubFile
		if m.Key == "" {
			m.Key = "files/" + uuid.NewString()
		}
		created, err := db.store.Create(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		if afterwards != "" {
			if _, err := db.pool.Exec(ctx, `UPDATE media SET `+afterwards+` WHERE id = $1`, created.ID); err != nil {
				t.Fatal(err)
			}
		}
		return created
	}
	want := map[uuid.UUID]bool{
		stored(media.Media{Status: media.StatusAttached}, "").ID:                                                                            true,
		stored(media.Media{Status: media.StatusPending}, "").ID:                                                                             true,
		stored(media.Media{Status: media.StatusDetached}, "").ID:                                                                            true,
		stored(media.Media{Status: media.StatusAttached}, "deleted_at = now()").ID:                                                          true,
		stored(media.Media{Status: media.StatusScanning}, "").ID:                                                                            false,
		stored(media.Media{Status: media.StatusPending}, "status = 'rejected', scan_result = 'infected', blob_purge_started_at = now()").ID: false,
		stored(media.Media{Status: media.StatusAttached}, "blob_purge_started_at = now()").ID:                                               false,
		stored(media.Media{Status: media.StatusAttached}, "blob_purge_started_at = now(), blob_purged_at = now()").ID:                       false,
		stored(media.Media{
			Status: media.StatusAttached, Key: "private/files/" + uuid.NewString(), Visibility: media.VisibilityPrivate,
			Encryption: &media.Encryption{Algorithm: "aes-256-gcm-chunked-v1", WrappedKey: "vault:v1:AAAA", KeyVersion: 1},
		}, "").ID: false,
	}
	rows, err := db.pool.Query(ctx, `SELECT id, `+media.ServableSQL("m")+` FROM media m`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id uuid.UUID
		var servable bool
		if err := rows.Scan(&id, &servable); err != nil {
			t.Fatal(err)
		}
		expected, ok := want[id]
		if !ok {
			continue
		}
		seen++
		got, err := db.store.GetIncludingDeleted(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if servable != expected || got.Servable() != expected {
			t.Errorf("%s (status %s, visibility %s, purge %v/%v): ServableSQL %v, Servable %v, want %v",
				id, got.Status, got.Visibility, got.BlobPurgeStartedAt, got.BlobPurgedAt, servable, got.Servable(), expected)
		}
	}
	if err := rows.Err(); err != nil || seen != len(want) {
		t.Fatalf("read %d of %d Media (err %v)", seen, len(want), err)
	}
}
