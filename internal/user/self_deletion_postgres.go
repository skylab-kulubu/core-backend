package user

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

const selfDeletionIntakeCols = `intake.idempotency_key_hash, intake.receipt_lookup_hash, intake.receipt_hash, intake.receipt_expires_at, intake.receipt_revoked_at, intake.created_at`
const selfDeletionRequestCols = `request.id, request.subject_id, request.requested_by, request.status, request.attempt_count, request.next_attempt_at, request.lease_until, request.lease_token, request.profile_media_id, request.last_error_code, request.created_at, request.updated_at, request.completed_at, request.platform_blocked_at`

func scanSelfDeletionRecord(row interface{ Scan(...any) error }) (SelfDeletionRecord, error) {
	var record SelfDeletionRecord
	var idempotencyHash, lookupHash, receiptHash []byte
	err := row.Scan(
		&record.Request.ID, &record.Request.SubjectID, &record.Request.RequestedBy,
		&record.Request.Status, &record.Request.AttemptCount, &record.Request.NextAttemptAt,
		&record.Request.LeaseUntil, &record.Request.LeaseToken, &record.Request.ProfileMediaID,
		&record.Request.LastErrorCode, &record.Request.CreatedAt, &record.Request.UpdatedAt,
		&record.Request.CompletedAt, &record.Request.PlatformBlockedAt,
		&idempotencyHash, &lookupHash, &receiptHash, &record.ReceiptExpiresAt,
		&record.ReceiptRevokedAt, &record.CreatedAt, &record.HasCompletedStep,
	)
	if err != nil {
		return SelfDeletionRecord{}, err
	}
	if len(idempotencyHash) != len(record.IdempotencyHash) || len(lookupHash) != len(record.ReceiptLookupHash) || len(receiptHash) != len(record.ReceiptHash) {
		return SelfDeletionRecord{}, ErrInvalid
	}
	copy(record.IdempotencyHash[:], idempotencyHash)
	copy(record.ReceiptLookupHash[:], lookupHash)
	copy(record.ReceiptHash[:], receiptHash)
	return record, nil
}

func selfDeletionRecordQuery(where string, suffix string) string {
	return `
		SELECT ` + selfDeletionRequestCols + `, ` + selfDeletionIntakeCols + `,
		       EXISTS (SELECT 1 FROM account_deletion_steps step WHERE step.request_id=request.id)
		FROM account_deletion_requests request
		JOIN account_deletion_self_intakes intake ON intake.request_id=request.id
		WHERE ` + where + ` ` + suffix
}

func (s *PostgresStore) RequestSelfDeletion(
	ctx context.Context,
	subjectID uuid.UUID,
	intake SelfDeletionIntake,
) (SelfDeletionRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SelfDeletionRecord{}, err
	}
	defer tx.Rollback(ctx)
	if err := subjectlock.Lock(ctx, tx, subjectID); err != nil {
		return SelfDeletionRecord{}, err
	}

	request, err := scanDeletionRequest(tx.QueryRow(ctx, `
		SELECT `+deletionRequestCols+`
		FROM account_deletion_requests
		WHERE subject_id=$1
		FOR UPDATE
	`, subjectID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return SelfDeletionRecord{}, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, created_at, updated_at)
			VALUES ($1, $2, $2)
			ON CONFLICT (id) DO NOTHING
		`, subjectID, intake.CreatedAt); err != nil {
			return SelfDeletionRecord{}, err
		}
		var state AccountState
		if err := tx.QueryRow(ctx, `SELECT account_state FROM users WHERE id=$1 FOR UPDATE`, subjectID).Scan(&state); err != nil {
			return SelfDeletionRecord{}, err
		}
		if state != AccountActive {
			return SelfDeletionRecord{}, ErrAccountBlocked
		}
		requestID := uuid.New()
		request, err = scanDeletionRequest(tx.QueryRow(ctx, `
			INSERT INTO account_deletion_requests (id, subject_id, next_attempt_at, created_at, updated_at)
			VALUES ($1, $2, $3, $3, $3)
			RETURNING `+deletionRequestCols,
			requestID, subjectID, intake.CreatedAt,
		))
		if subjectlock.IsInactiveAccountReference(err) {
			return SelfDeletionRecord{}, ErrAccountBlocked
		}
		if err != nil {
			return SelfDeletionRecord{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_deletion_outbox (id, request_id, subject_id, available_at, created_at)
			VALUES ($1, $2, $3, 'infinity'::timestamptz, $4)
		`, uuid.New(), request.ID, request.SubjectID, intake.CreatedAt); err != nil {
			return SelfDeletionRecord{}, err
		}
		if tag, err := tx.Exec(ctx, `
			UPDATE users
			SET account_state='deletion_pending', deletion_requested_at=COALESCE(deletion_requested_at, $2), updated_at=$2
			WHERE id=$1 AND account_state='active'
		`, subjectID, intake.CreatedAt); err != nil {
			return SelfDeletionRecord{}, err
		} else if tag.RowsAffected() != 1 {
			return SelfDeletionRecord{}, ErrAccountBlocked
		}
	}

	var existingIdempotency []byte
	err = tx.QueryRow(ctx, `
		SELECT idempotency_key_hash
		FROM account_deletion_self_intakes
		WHERE request_id=$1
		FOR UPDATE
	`, request.ID).Scan(&existingIdempotency)
	switch {
	case err == nil:
		if len(existingIdempotency) != len(intake.IdempotencyHash) || subtle.ConstantTimeCompare(existingIdempotency, intake.IdempotencyHash[:]) != 1 {
			return SelfDeletionRecord{}, ErrSelfDeletionIdempotencyConflict
		}
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_deletion_self_intakes (
				request_id, idempotency_key_hash, receipt_lookup_hash, receipt_hash, receipt_expires_at, created_at
			) VALUES ($1, $2, $3, $4, $5, $6)
		`, request.ID, intake.IdempotencyHash[:], intake.ReceiptLookupHash[:], intake.ReceiptHash[:], intake.ReceiptExpiresAt, intake.CreatedAt); err != nil {
			if isUnique(err) {
				return SelfDeletionRecord{}, ErrSelfDeletionIdempotencyConflict
			}
			return SelfDeletionRecord{}, err
		}
	default:
		return SelfDeletionRecord{}, err
	}

	record, err := scanSelfDeletionRecord(tx.QueryRow(ctx,
		selfDeletionRecordQuery("request.id=$1", ""), request.ID,
	))
	if err != nil {
		return SelfDeletionRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SelfDeletionRecord{}, err
	}
	return record, nil
}

func (s *PostgresStore) SelfDeletionByReceiptLookup(ctx context.Context, receiptLookupHash [32]byte) (SelfDeletionRecord, error) {
	record, err := scanSelfDeletionRecord(s.pool.QueryRow(ctx,
		selfDeletionRecordQuery("intake.receipt_lookup_hash=$1", ""), receiptLookupHash[:],
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return SelfDeletionRecord{}, ErrNotFound
	}
	return record, err
}

func (s *PostgresStore) RetrySelfDeletion(ctx context.Context, receiptLookupHash [32]byte, now time.Time) (SelfDeletionRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SelfDeletionRecord{}, err
	}
	defer tx.Rollback(ctx)
	record, err := scanSelfDeletionRecord(tx.QueryRow(ctx,
		selfDeletionRecordQuery(
			"intake.receipt_lookup_hash=$1 AND intake.receipt_revoked_at IS NULL AND intake.receipt_expires_at>$2",
			"FOR UPDATE OF request, intake",
		), receiptLookupHash[:], now,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return SelfDeletionRecord{}, ErrNotFound
	}
	if err != nil {
		return SelfDeletionRecord{}, err
	}
	if record.Request.Status == DeletionRequestManualIntervention {
		if _, err := tx.Exec(ctx, `
			UPDATE account_deletion_requests
			SET status='pending', attempt_count=0, next_attempt_at=$2,
				lease_until=NULL, lease_token=NULL, last_error_code='', updated_at=$2
			WHERE id=$1 AND status='manual_intervention'
		`, record.Request.ID, now); err != nil {
			return SelfDeletionRecord{}, err
		}
		record.Request.Status = DeletionRequestPending
		record.Request.AttemptCount = 0
		record.Request.NextAttemptAt = now
		record.Request.LeaseUntil = nil
		record.Request.LeaseToken = nil
		record.Request.LastErrorCode = ""
		record.Request.UpdatedAt = now
	}
	if err := tx.Commit(ctx); err != nil {
		return SelfDeletionRecord{}, err
	}
	return record, nil
}

// RevokeSelfDeletionReceipt is an internal operational primitive. It is not
// exposed by the HTTP lifecycle interface and never changes account state.
func (s *PostgresStore) RevokeSelfDeletionReceipt(ctx context.Context, requestID uuid.UUID, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE account_deletion_self_intakes
		SET receipt_revoked_at=COALESCE(receipt_revoked_at, $2)
		WHERE request_id=$1
	`, requestID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
