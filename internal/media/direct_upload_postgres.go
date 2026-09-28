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
	d.part_size, d.object_key, d.multipart_upload_id, d.expires_at, d.claim_until`

func scanDirectUpload(row pgx.Row) (DirectUploadRecord, error) {
	var rec DirectUploadRecord
	err := row.Scan(&rec.ID, &rec.UploaderID, &rec.Purpose, &rec.Name, &rec.Size, &rec.MaxBytes, &rec.Types,
		&rec.PartSize, &rec.Key, &rec.MultipartID, &rec.ExpiresAt, &rec.ClaimUntil)
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

// lockPendingRow locks the staging row of a Direct upload's pending object
// without waiting: the lock the staging sweeper and account erasure take
// first. held is false when another transaction holds it (the sweeper
// deleting it, erasure checking it); ErrNotFound when the upload is gone.
func lockPendingRow(ctx context.Context, tx pgx.Tx, id uuid.UUID) (pendingKey string, held bool, _ error) {
	if err := tx.QueryRow(ctx, `SELECT object_key FROM media_direct_uploads WHERE id=$1`, id).Scan(&pendingKey); errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrNotFound
	} else if err != nil {
		return "", false, err
	}
	var locked string
	err := tx.QueryRow(ctx, `SELECT object_key FROM media_upload_staging WHERE object_key=$1 FOR UPDATE SKIP LOCKED`, pendingKey).Scan(&locked)
	if err == nil {
		return pendingKey, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM media_upload_staging WHERE object_key=$1)`, pendingKey).Scan(&exists); err != nil {
		return "", false, err
	}
	if !exists {
		return "", false, ErrNotFound
	}
	return pendingKey, false, nil
}

// ClaimDirectUpload claims the uploader's open upload for a completion, in
// one short transaction: its record is marked with the claim and its lease,
// finalKey is staged for the copy until the lease ends, and so is the
// pending object (the sweeper and account erasure leave both alone until
// then). Nothing is locked once it returns.
func (s *PostgresStore) ClaimDirectUpload(ctx context.Context, id, uploader uuid.UUID, finalKey string, now, until time.Time) (DirectUploadClaim, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DirectUploadClaim{}, err
	}
	defer tx.Rollback(ctx)
	pendingKey, held, err := lockPendingRow(ctx, tx, id)
	if err != nil {
		return DirectUploadClaim{}, err
	}
	if !held {
		return DirectUploadClaim{}, ErrDirectUploadCompleting
	}
	rec, err := scanDirectUpload(tx.QueryRow(ctx, `
		SELECT `+directUploadCols+`
		FROM media_direct_uploads d
		JOIN media_upload_staging s ON s.object_key = d.object_key
		WHERE d.id = $1
		FOR UPDATE OF d
	`, id))
	if err != nil {
		return DirectUploadClaim{}, err
	}
	if rec.UploaderID != uploader || rec.MultipartID == "" || !now.Before(rec.ExpiresAt) {
		return DirectUploadClaim{}, ErrNotFound
	}
	if rec.ClaimUntil != nil && rec.ClaimUntil.After(now) {
		return DirectUploadClaim{}, ErrDirectUploadCompleting
	}
	claim := DirectUploadClaim{Upload: rec, ID: uuid.New(), Until: until, FinalKey: finalKey}
	_, err = tx.Exec(ctx, `
		INSERT INTO media_upload_staging (object_key, subject_id, cleanup_after) VALUES ($1, $2, $3)
	`, finalKey, uploader, until)
	if subjectlock.IsInactiveAccountReference(err) {
		return DirectUploadClaim{}, ErrForbidden
	}
	if err != nil {
		return DirectUploadClaim{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE media_direct_uploads SET claim_id=$2, claimed_at=$3, claim_until=$4, final_key=$5 WHERE id=$1
	`, id, claim.ID, now, until, finalKey); err != nil {
		return DirectUploadClaim{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE media_upload_staging SET cleanup_after=$2 WHERE object_key=$1`, pendingKey, until); err != nil {
		return DirectUploadClaim{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DirectUploadClaim{}, err
	}
	claim.Upload.ClaimUntil = &claim.Until
	return claim, nil
}

// ReleaseDirectUpload lets go of a claim that copied nothing, in one short
// transaction: the upload is open again until its own expiry, and its final
// key is no longer staged. A claim already lost changes nothing.
func (s *PostgresStore) ReleaseDirectUpload(ctx context.Context, claim DirectUploadClaim) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	pendingKey, held, err := lockPendingRow(ctx, tx, claim.Upload.ID)
	if errors.Is(err, ErrNotFound) || (err == nil && !held) {
		// Gone, or the sweeper has it: the lease ran out.
		return nil
	}
	if err != nil {
		return err
	}
	var expires time.Time
	err = tx.QueryRow(ctx, `
		UPDATE media_direct_uploads SET claim_id=NULL, claimed_at=NULL, claim_until=NULL, final_key=NULL
		WHERE id=$1 AND claim_id=$2
		RETURNING expires_at
	`, claim.Upload.ID, claim.ID).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM media_upload_staging WHERE object_key=$1`, claim.FinalKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE media_upload_staging SET cleanup_after=$2 WHERE object_key=$1`, pendingKey, expires); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FinishDirectUpload creates the upload's Media (item, stored at the claim's
// final key) with the upload's id and ends the upload, in one short
// transaction: the final key's staging row and the upload go, and the
// pending object is left to the cleanup. ErrDirectUploadClaimLost when the
// claim is no longer the upload's, or its lease ran out.
func (s *PostgresStore) FinishDirectUpload(ctx context.Context, claim DirectUploadClaim, item Media, now time.Time) (Media, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Media{}, err
	}
	defer tx.Rollback(ctx)
	pendingKey, held, err := lockPendingRow(ctx, tx, claim.Upload.ID)
	if errors.Is(err, ErrNotFound) || (err == nil && !held) {
		return Media{}, ErrDirectUploadClaimLost
	}
	if err != nil {
		return Media{}, err
	}
	var claimID *uuid.UUID
	var until *time.Time
	if err := tx.QueryRow(ctx, `SELECT claim_id, claim_until FROM media_direct_uploads WHERE id=$1 FOR UPDATE`, claim.Upload.ID).Scan(&claimID, &until); errors.Is(err, pgx.ErrNoRows) {
		return Media{}, ErrDirectUploadClaimLost
	} else if err != nil {
		return Media{}, err
	}
	if claimID == nil || *claimID != claim.ID || until == nil || !until.After(now) {
		return Media{}, ErrDirectUploadClaimLost
	}
	item.ID, item.Key = claim.Upload.ID, claim.FinalKey
	created, err := publishStaged(ctx, tx, item)
	if err != nil {
		return Media{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM media_direct_uploads WHERE id=$1`, claim.Upload.ID); err != nil {
		return Media{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE media_upload_staging SET cleanup_after=LEAST(cleanup_after, $2) WHERE object_key=$1
	`, pendingKey, now); err != nil {
		return Media{}, err
	}
	return s.commitPublication(ctx, tx, created)
}

// EndDirectUpload ends an upload without a Media, once storage was asked to
// delete its objects, in one short transaction. The record goes while it is
// unclaimed (claimID Nil) or still under claimID, and with it the staging
// row of its pending object; the staging row of finalKey (the claim's copy;
// "" for none) goes in any case. removed false (a delete failed) leaves
// those staging rows ready for the sweeper instead.
func (s *PostgresStore) EndDirectUpload(ctx context.Context, id, claimID uuid.UUID, finalKey string, removed bool, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var keys []string
	if finalKey != "" {
		keys = append(keys, finalKey)
	}
	pendingKey, held, err := lockPendingRow(ctx, tx, id)
	switch {
	case errors.Is(err, ErrNotFound), err == nil && !held:
		// Ended already, or the sweeper has it.
	case err != nil:
		return err
	default:
		var claim *uuid.UUID
		if claimID != uuid.Nil {
			claim = &claimID
		}
		tag, err := tx.Exec(ctx, `DELETE FROM media_direct_uploads WHERE id=$1 AND claim_id IS NOT DISTINCT FROM $2`, id, claim)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			keys = append(keys, pendingKey)
		}
	}
	if removed {
		_, err = tx.Exec(ctx, `DELETE FROM media_upload_staging WHERE object_key = ANY($1)`, keys)
	} else {
		_, err = tx.Exec(ctx, `UPDATE media_upload_staging SET cleanup_after=LEAST(cleanup_after, $2) WHERE object_key = ANY($1)`, keys, now)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
