package user

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func scanDeletionRequest(row interface{ Scan(...any) error }) (DeletionRequest, error) {
	var request DeletionRequest
	err := row.Scan(
		&request.ID, &request.SubjectID, &request.RequestedBy, &request.Status, &request.AttemptCount,
		&request.NextAttemptAt, &request.LeaseUntil, &request.LeaseToken, &request.ProfileMediaID,
		&request.LastErrorCode, &request.CreatedAt,
		&request.UpdatedAt, &request.CompletedAt,
	)
	return request, err
}

const deletionRequestCols = `id, subject_id, requested_by, status, attempt_count, next_attempt_at, lease_until, lease_token, profile_media_id, last_error_code, created_at, updated_at, completed_at`

func (s *PostgresStore) DeletionRequest(ctx context.Context, subjectID uuid.UUID) (DeletionRequest, error) {
	request, err := scanDeletionRequest(s.pool.QueryRow(ctx, `SELECT `+deletionRequestCols+` FROM account_deletion_requests WHERE subject_id = $1`, subjectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeletionRequest{}, ErrNotFound
	}
	return request, err
}

func (s *PostgresStore) ClaimDeletionRequest(ctx context.Context, now time.Time, lease time.Duration) (DeletionRequest, bool, error) {
	leaseToken := uuid.New()
	request, err := scanDeletionRequest(s.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id
			FROM account_deletion_requests
			WHERE next_attempt_at <= $1
			  AND (
				status = 'pending'
				OR (status = 'processing' AND lease_until IS NOT NULL AND lease_until <= $1)
			  )
			ORDER BY next_attempt_at, created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE account_deletion_requests request
		SET status = 'processing', attempt_count = attempt_count + 1,
			lease_until = $1 + $2::interval, lease_token = $3, updated_at = $1
		FROM candidate
		WHERE request.id = candidate.id
		RETURNING request.id, request.subject_id, request.requested_by, request.status,
			request.attempt_count, request.next_attempt_at, request.lease_until,
			request.lease_token, request.profile_media_id, request.last_error_code,
			request.created_at, request.updated_at, request.completed_at
	`, now, lease.String(), leaseToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeletionRequest{}, false, nil
	}
	return request, err == nil, err
}

func (s *PostgresStore) CompletedDeletionSteps(ctx context.Context, requestID, leaseToken uuid.UUID) (map[DeletionStep]bool, error) {
	if err := s.requireDeletionLease(ctx, requestID, leaseToken); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT step FROM account_deletion_steps WHERE request_id = $1`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[DeletionStep]bool)
	for rows.Next() {
		var step DeletionStep
		if err := rows.Scan(&step); err != nil {
			return nil, err
		}
		out[step] = true
	}
	return out, rows.Err()
}

func (s *PostgresStore) CompleteDeletionStep(ctx context.Context, requestID, leaseToken uuid.UUID, step DeletionStep, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		WITH fenced AS (
			UPDATE account_deletion_requests
			SET profile_media_id = CASE WHEN $3 = 'erase_profile_media' THEN NULL ELSE profile_media_id END,
				updated_at = $4
			WHERE id = $1 AND status = 'processing' AND lease_token = $2
			RETURNING id
		)
		INSERT INTO account_deletion_steps (request_id, step, completed_at)
		SELECT id, $3, $4 FROM fenced
		ON CONFLICT (request_id, step) DO UPDATE
		SET completed_at = account_deletion_steps.completed_at
	`, requestID, leaseToken, step, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresStore) RetryDeletionRequest(ctx context.Context, requestID, leaseToken uuid.UUID, next time.Time, code string, manual, refundAttempt bool) error {
	status := DeletionRequestPending
	if manual {
		status = DeletionRequestManualIntervention
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE account_deletion_requests
		SET status = $3, next_attempt_at = $4, lease_until = NULL, lease_token = NULL,
			last_error_code = $5, updated_at = $4,
			attempt_count = CASE WHEN $6 THEN GREATEST(attempt_count - 1, 0) ELSE attempt_count END
		WHERE id = $1 AND status = 'processing' AND lease_token = $2
	`, requestID, leaseToken, status, next, code, refundAttempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresStore) CompleteDeletionRequest(ctx context.Context, requestID, leaseToken uuid.UUID, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE account_deletion_requests
		SET status = 'completed', lease_until = NULL, lease_token = NULL, last_error_code = '',
			completed_at = COALESCE(completed_at, $3), updated_at = $3
		WHERE id = $1 AND status = 'processing' AND lease_token = $2
	`, requestID, leaseToken, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresStore) ProfileMediaForDeletion(ctx context.Context, requestID uuid.UUID) (*uuid.UUID, error) {
	var mediaID *uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT profile_media_id FROM account_deletion_requests WHERE id = $1`, requestID).Scan(&mediaID); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return mediaID, nil
}

func (s *PostgresStore) requireDeletionLease(ctx context.Context, requestID, leaseToken uuid.UUID) error {
	var held bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM account_deletion_requests
			WHERE id = $1 AND status = 'processing' AND lease_token = $2
		)
	`, requestID, leaseToken).Scan(&held); err != nil {
		return err
	}
	if !held {
		return ErrLeaseLost
	}
	return nil
}
