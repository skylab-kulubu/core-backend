package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const mediaVideoFaststartVersion = "20260929120000"

// videoMedia stores a video Media with the faststart columns given.
func videoMedia(t *testing.T, pool *pgxpool.Pool, set string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{Email: uploader.String() + "@example.test"}); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, purpose, status)
		VALUES ($1, 'a.mp4', 'video/mp4', 'videos/a.mp4', 10, $2, 'FILE', 'video', 'pending')`, id, uploader); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `UPDATE media SET `+set+` WHERE id = $1`, append([]any{id}, args...)...)
	return err
}

// videoFaststartHolds reports whether a video's faststart state is one of
// the rewrite's, its claim whole, its attempts counted from zero.
func videoFaststartHolds(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, ok := range []string{
		`video_faststart = NULL`, `video_faststart = 'moved'`, `video_faststart = 'done'`, `video_faststart = 'not_needed'`, `video_faststart = 'failed'`,
		`video_faststart_attempts = 3, video_faststart_retry_at = now()`,
		`video_faststart_claim_id = gen_random_uuid(), video_faststart_claimed_until = now()`,
	} {
		if err := videoMedia(t, pool, ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, refused := range []string{
		`video_faststart = 'remuxing'`, `video_faststart_attempts = -1`,
		`video_faststart_claim_id = gen_random_uuid()`, `video_faststart_claimed_until = now()`,
	} {
		if err := videoMedia(t, pool, refused); err == nil {
			t.Errorf("%s was stored", refused)
		}
	}
}

// A video Media waits for its faststart rewrite (NULL), is moved to its
// copy, and ends done, not needed or failed; a worker's claim is its id and
// lease together.
func TestMediaVideoFaststartStates(t *testing.T) {
	pool := postgresPool(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	videoFaststartHolds(t, pool)
}

// A database that lost part of the faststart schema is not taken for
// migrated: the migration runs again and puts it back.
func TestApplyRepairsTheVideoFaststartSchema(t *testing.T) {
	for name, damage := range map[string]string{
		"a column": `DELETE FROM schema_migrations WHERE version = ` + mediaVideoFaststartVersion + `;
			ALTER TABLE media DROP COLUMN video_faststart_claimed_until CASCADE`,
		"the check": `DELETE FROM schema_migrations WHERE version = ` + mediaVideoFaststartVersion + `;
			ALTER TABLE media DROP CONSTRAINT media_video_faststart_check`,
		"a weaker check": `DELETE FROM schema_migrations WHERE version = ` + mediaVideoFaststartVersion + `;
			ALTER TABLE media DROP CONSTRAINT media_video_faststart_check,
				ADD CONSTRAINT media_video_faststart_check CHECK (video_faststart_attempts >= 0)`,
		"the due index": `DELETE FROM schema_migrations WHERE version = ` + mediaVideoFaststartVersion + `;
			DROP INDEX media_video_faststart_due_idx`,
	} {
		t.Run(name, func(t *testing.T) {
			pool := postgresPool(t)
			ctx := context.Background()
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, damage); err != nil {
				t.Fatal(err)
			}
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			videoFaststartHolds(t, pool)
			var indexed bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass('public.media_video_faststart_due_idx') IS NOT NULL`).Scan(&indexed); err != nil || !indexed {
				t.Fatalf("due index back %v, err %v", indexed, err)
			}
		})
	}
}
