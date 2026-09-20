package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

const uploadStagingRetryDelay = time.Hour

var ErrStagedUploadInFlight = errors.New("media: staged upload still in flight")

type stagedUploadDeferredError struct {
	cause   error
	retryAt time.Time
}

func (e stagedUploadDeferredError) Error() string      { return e.cause.Error() }
func (e stagedUploadDeferredError) Unwrap() error      { return e.cause }
func (e stagedUploadDeferredError) RetryAt() time.Time { return e.retryAt }
func deferStagedUpload(cause error, retryAt time.Time) error {
	return stagedUploadDeferredError{cause: cause, retryAt: retryAt}
}

func (s *PostgresStore) StageUpload(ctx context.Context, key string, subjectID uuid.UUID, cleanupAfter time.Time) error {
	if key == "" || subjectID == uuid.Nil || cleanupAfter.IsZero() {
		return ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO media_upload_staging (object_key, subject_id, cleanup_after)
		VALUES ($1, $2, $3)
	`, key, subjectID, cleanupAfter)
	if subjectlock.IsInactiveAccountReference(err) {
		return ErrForbidden
	}
	return err
}

func (s *PostgresStore) ReadyStagedUploadForCleanup(ctx context.Context, key string, cleanupAfter time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE media_upload_staging
		SET cleanup_after=LEAST(cleanup_after, $2)
		WHERE object_key=$1
	`, key, cleanupAfter)
	return err
}

func (s *PostgresStore) CancelStagedUpload(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM media_upload_staging WHERE object_key=$1`, key)
	return err
}

func (s *PostgresStore) CreateStaged(ctx context.Context, item Media) (Media, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Media{}, err
	}
	defer tx.Rollback(ctx)

	var key string
	var subjectID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT object_key, subject_id FROM media_upload_staging WHERE object_key=$1 FOR UPDATE
	`, item.Key).Scan(&key, &subjectID); errors.Is(err, pgx.ErrNoRows) {
		return Media{}, ErrInvalid
	} else if err != nil {
		return Media{}, err
	}
	if subjectID != item.UploadedBy {
		return Media{}, ErrForbidden
	}
	if item.ID == uuid.Nil {
		item.ID = uuid.New()
	}
	if item.CoverColors == nil {
		item.CoverColors = []string{}
	}
	created, err := scanMedia(tx.QueryRow(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, cover_colors, cover_colors_computed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+mediaCols, item.ID, item.Name, item.Type, item.Key, item.Size, item.UploadedBy, item.Kind, item.CoverColors, item.CoverColorsComputed))
	if subjectlock.IsInactiveAccountReference(err) {
		return Media{}, ErrForbidden
	}
	if err != nil {
		return Media{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM media_upload_staging WHERE object_key=$1`, item.Key); err != nil {
		return Media{}, err
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		published, lookupErr := s.GetIncludingDeleted(reconcileCtx, item.ID)
		if lookupErr == nil && published.Key == item.Key {
			return published, nil
		}
		if errors.Is(lookupErr, ErrNotFound) {
			return Media{}, commitErr
		}
		return Media{}, errors.Join(ErrPublicationUncertain, commitErr, lookupErr)
	}
	return created, nil
}

// PurgeNextStagedUpload holds the staging row lock across the final reference
// check and the idempotent object deletion. CreateStaged uses the same row lock,
// so a live upload can either publish metadata or be cleaned up, never both.
func (s *PostgresStore) PurgeNextStagedUpload(ctx context.Context, now time.Time, purge func(key string) error) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var key string
	if err := tx.QueryRow(ctx, `
		SELECT object_key
		FROM media_upload_staging
		WHERE cleanup_after <= $1
		ORDER BY cleanup_after, created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`, now).Scan(&key); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	var referenced bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM media WHERE file_url=$1)`, key).Scan(&referenced); err != nil {
		return true, err
	}
	if referenced {
		if _, err := tx.Exec(ctx, `DELETE FROM media_upload_staging WHERE object_key=$1`, key); err != nil {
			return true, err
		}
		return true, tx.Commit(ctx)
	}

	if err := purge(key); err != nil {
		if _, updateErr := tx.Exec(ctx, `
			UPDATE media_upload_staging
			SET attempt_count=attempt_count+1,
				cleanup_after=$2,
				last_error_code='blob_delete_failed'
			WHERE object_key=$1
		`, key, now.Add(uploadStagingRetryDelay)); updateErr != nil {
			return true, errors.Join(err, updateErr)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return true, errors.Join(err, commitErr)
		}
		return true, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM media_upload_staging WHERE object_key=$1`, key); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

// PurgeNextSubjectStagedUpload is the account-erasure fence. Unlike the
// ordinary sweeper it waits for an in-progress publication row lock and never
// skips a subject row. A future cleanup_after is an active upload lease, so the
// deletion saga must retry rather than report completion.
func (s *PostgresStore) PurgeNextSubjectStagedUpload(ctx context.Context, subjectID uuid.UUID, now time.Time, purge func(key string) error) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var key string
	var cleanupAfter time.Time
	if err := tx.QueryRow(ctx, `
		SELECT object_key, cleanup_after
		FROM media_upload_staging
		WHERE subject_id=$1
		ORDER BY cleanup_after, created_at
		FOR UPDATE
		LIMIT 1
	`, subjectID).Scan(&key, &cleanupAfter); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if cleanupAfter.After(now) {
		return true, deferStagedUpload(ErrStagedUploadInFlight, cleanupAfter)
	}

	var referenced bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM media WHERE file_url=$1)`, key).Scan(&referenced); err != nil {
		return true, err
	}
	if referenced {
		if _, err := tx.Exec(ctx, `DELETE FROM media_upload_staging WHERE object_key=$1`, key); err != nil {
			return true, err
		}
		return true, tx.Commit(ctx)
	}
	if err := purge(key); err != nil {
		retryAt := now.Add(uploadStagingRetryDelay)
		if _, updateErr := tx.Exec(ctx, `
			UPDATE media_upload_staging
			SET attempt_count=attempt_count+1,
				cleanup_after=$2,
				last_error_code='account_blob_delete_failed'
			WHERE object_key=$1
		`, key, retryAt); updateErr != nil {
			return true, errors.Join(err, updateErr)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return true, errors.Join(err, commitErr)
		}
		return true, deferStagedUpload(err, retryAt)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM media_upload_staging WHERE object_key=$1`, key); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}
