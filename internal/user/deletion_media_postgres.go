package user

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MediaForDeletion returns the Media the request's erase_profile_media step
// still has to erase: the profile picture anonymize_core kept for it
// (profile_media_id) first, unless its object is purged already, then the
// personal Media anonymize_core recorded (account_deletion_media), in id
// order. A recorded Media leaves the list in the transaction that purges it,
// and the profile picture as its object goes, so a rerun gets only what is
// left and every Media the step erases is progress.
func (s *PostgresStore) MediaForDeletion(ctx context.Context, requestID uuid.UUID) ([]uuid.UUID, error) {
	var profile *uuid.UUID
	var recorded []uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT CASE WHEN picture.blob_purged_at IS NULL THEN request.profile_media_id END,
			COALESCE(ARRAY(
				SELECT record.media_id FROM account_deletion_media record
				WHERE record.request_id = request.id AND record.media_id IS DISTINCT FROM request.profile_media_id
				ORDER BY record.media_id
			), '{}')
		FROM account_deletion_requests request
		LEFT JOIN media picture ON picture.id = request.profile_media_id
		WHERE request.id = $1
	`, requestID).Scan(&profile, &recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if profile == nil {
		return recorded, nil
	}
	return append([]uuid.UUID{*profile}, recorded...), nil
}
