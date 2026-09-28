package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

const directUploadCols = `d.id, s.subject_id, d.purpose, d.file_name, d.declared_size, d.max_bytes, d.allowed_types,
	d.part_size, d.object_key, d.multipart_upload_id, d.expires_at`

func scanDirectUpload(row pgx.Row) (DirectUploadRecord, error) {
	var rec DirectUploadRecord
	err := row.Scan(&rec.ID, &rec.UploaderID, &rec.Purpose, &rec.Name, &rec.Size, &rec.MaxBytes, &rec.Types,
		&rec.PartSize, &rec.Key, &rec.MultipartID, &rec.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DirectUploadRecord{}, ErrNotFound
	}
	return rec, err
}

func (s *PostgresStore) StageDirectUpload(ctx context.Context, rec DirectUploadRecord, maxOpen int, now time.Time) error {
	if rec.ID == uuid.Nil || rec.UploaderID == uuid.Nil || rec.Key == "" || rec.ExpiresAt.IsZero() {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if maxOpen > 0 {
		// One start at a time per person, so two cannot both see room for
		// one more.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('media_direct_uploads:' || $1::text, 0))`, rec.UploaderID); err != nil {
			return err
		}
		var open int
		var next *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT count(*), min(d.expires_at)
			FROM media_direct_uploads d
			JOIN media_upload_staging s ON s.object_key = d.object_key
			WHERE s.subject_id = $1 AND d.expires_at > $2
		`, rec.UploaderID, now).Scan(&open, &next); err != nil {
			return err
		}
		if open >= maxOpen && next != nil {
			return &TooManyOpenDirectUploads{NextExpiry: *next}
		}
	}
	// The staging row first: its guard refuses an account that is not
	// active, and it is what every cleanup deletes the pending object by.
	_, err = tx.Exec(ctx, `
		INSERT INTO media_upload_staging (object_key, subject_id, cleanup_after)
		VALUES ($1, $2, $3)
	`, rec.Key, rec.UploaderID, rec.ExpiresAt)
	if subjectlock.IsInactiveAccountReference(err) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO media_direct_uploads (id, object_key, purpose, file_name, declared_size, max_bytes, allowed_types, part_size, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, rec.ID, rec.Key, rec.Purpose, rec.Name, rec.Size, rec.MaxBytes, rec.Types, rec.PartSize, rec.ExpiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) SetDirectUploadMultipart(ctx context.Context, id uuid.UUID, multipartID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE media_direct_uploads SET multipart_upload_id=$2 WHERE id=$1`, id, multipartID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) GetDirectUpload(ctx context.Context, id uuid.UUID) (DirectUploadRecord, error) {
	return scanDirectUpload(s.pool.QueryRow(ctx, `
		SELECT `+directUploadCols+`
		FROM media_direct_uploads d
		JOIN media_upload_staging s ON s.object_key = d.object_key
		WHERE d.id = $1
	`, id))
}

// lockDirectUpload locks a Direct upload for its end: the staging row of its
// pending object first, then its record, the order the staging sweeper and
// account erasure take them in (they lock the staging row, and deleting it
// deletes the record). ErrNotFound when the upload is gone.
func lockDirectUpload(ctx context.Context, tx pgx.Tx, id uuid.UUID) (DirectUploadRecord, error) {
	var pendingKey string
	if err := tx.QueryRow(ctx, `SELECT object_key FROM media_direct_uploads WHERE id=$1`, id).Scan(&pendingKey); errors.Is(err, pgx.ErrNoRows) {
		return DirectUploadRecord{}, ErrNotFound
	} else if err != nil {
		return DirectUploadRecord{}, err
	}
	var locked string
	if err := tx.QueryRow(ctx, `SELECT object_key FROM media_upload_staging WHERE object_key=$1 FOR UPDATE`, pendingKey).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return DirectUploadRecord{}, ErrNotFound
	} else if err != nil {
		return DirectUploadRecord{}, err
	}
	return scanDirectUpload(tx.QueryRow(ctx, `
		SELECT `+directUploadCols+`
		FROM media_direct_uploads d
		JOIN media_upload_staging s ON s.object_key = d.object_key
		WHERE d.id = $1
		FOR UPDATE OF d
	`, id))
}

// closeDirectUploadTx removes the upload's record and leaves its pending
// object to the cleanup from now on: the caller deletes it at once, and the
// staging sweeper retries should that fail.
func closeDirectUploadTx(ctx context.Context, tx pgx.Tx, rec DirectUploadRecord, now time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM media_direct_uploads WHERE id=$1`, rec.ID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE media_upload_staging SET cleanup_after=LEAST(cleanup_after, $2) WHERE object_key=$1
	`, rec.Key, now)
	return err
}

func (s *PostgresStore) DropDirectUpload(ctx context.Context, id uuid.UUID, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rec, err := lockDirectUpload(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := closeDirectUploadTx(ctx, tx, rec, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) BeginDirectCompletion(ctx context.Context, id uuid.UUID) (DirectCompletion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := lockDirectUpload(ctx, tx, id)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return &pgDirectCompletion{store: s, tx: tx, rec: rec}, nil
}

// pgDirectCompletion holds a Direct upload's rows locked in one open
// transaction until the completion publishes, drops or releases it.
type pgDirectCompletion struct {
	store *PostgresStore
	tx    pgx.Tx
	rec   DirectUploadRecord
}

func (c *pgDirectCompletion) Upload() DirectUploadRecord { return c.rec }

func (c *pgDirectCompletion) Publish(ctx context.Context, item Media, now time.Time) (Media, error) {
	defer c.tx.Rollback(ctx)
	item.ID = c.rec.ID
	created, err := publishStaged(ctx, c.tx, item)
	if err != nil {
		return Media{}, err
	}
	if err := closeDirectUploadTx(ctx, c.tx, c.rec, now); err != nil {
		return Media{}, err
	}
	return c.store.commitPublication(ctx, c.tx, created)
}

func (c *pgDirectCompletion) Drop(ctx context.Context, now time.Time) error {
	defer c.tx.Rollback(ctx)
	if err := closeDirectUploadTx(ctx, c.tx, c.rec, now); err != nil {
		return err
	}
	return c.tx.Commit(ctx)
}

func (c *pgDirectCompletion) Release(ctx context.Context) {
	_ = c.tx.Rollback(ctx)
}
