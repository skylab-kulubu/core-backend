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

func (s *PostgresStore) StageDirectUpload(ctx context.Context, rec DirectUploadRecord) error {
	if rec.ID == uuid.Nil || rec.UploaderID == uuid.Nil || rec.Key == "" || rec.ExpiresAt.IsZero() {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
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
	var rec DirectUploadRecord
	err := s.pool.QueryRow(ctx, `
		SELECT `+directUploadCols+`
		FROM media_direct_uploads d
		JOIN media_upload_staging s ON s.object_key = d.object_key
		WHERE d.id = $1
	`, id).Scan(&rec.ID, &rec.UploaderID, &rec.Purpose, &rec.Name, &rec.Size, &rec.MaxBytes, &rec.Types,
		&rec.PartSize, &rec.Key, &rec.MultipartID, &rec.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DirectUploadRecord{}, ErrNotFound
	}
	return rec, err
}

// lockDirectUpload locks a Direct upload for its end: the staging row of its
// pending object first, then its record, the order the staging sweeper and
// account erasure take them in (they lock the staging row, and deleting it
// deletes the record). ErrNotFound when the upload is gone.
func lockDirectUpload(ctx context.Context, tx pgx.Tx, id uuid.UUID) (pendingKey string, _ error) {
	if err := tx.QueryRow(ctx, `SELECT object_key FROM media_direct_uploads WHERE id=$1`, id).Scan(&pendingKey); errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	var locked string
	if err := tx.QueryRow(ctx, `SELECT object_key FROM media_upload_staging WHERE object_key=$1 FOR UPDATE`, pendingKey).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `SELECT object_key FROM media_direct_uploads WHERE id=$1 FOR UPDATE`, id).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	return pendingKey, nil
}

// endDirectUpload removes the upload's record and leaves its pending object
// to the cleanup from now on: the caller deletes it at once, and the staging
// sweeper retries should that fail.
func endDirectUpload(ctx context.Context, tx pgx.Tx, id uuid.UUID, pendingKey string, now time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM media_direct_uploads WHERE id=$1`, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE media_upload_staging SET cleanup_after=LEAST(cleanup_after, $2) WHERE object_key=$1
	`, pendingKey, now)
	return err
}

func (s *PostgresStore) PublishDirectUpload(ctx context.Context, id uuid.UUID, item Media, now time.Time) (Media, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Media{}, err
	}
	defer tx.Rollback(ctx)
	pendingKey, err := lockDirectUpload(ctx, tx, id)
	if err != nil {
		return Media{}, err
	}
	item.ID = id
	created, err := publishStaged(ctx, tx, item)
	if err != nil {
		return Media{}, err
	}
	if err := endDirectUpload(ctx, tx, id, pendingKey, now); err != nil {
		return Media{}, err
	}
	return s.commitPublication(ctx, tx, created)
}

func (s *PostgresStore) DropDirectUpload(ctx context.Context, id uuid.UUID, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	pendingKey, err := lockDirectUpload(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := endDirectUpload(ctx, tx, id, pendingKey, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
