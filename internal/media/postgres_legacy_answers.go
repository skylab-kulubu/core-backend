package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// legacyAnswerSharedSQL holds for a Media something other than a Skyforms
// answer uses: another Media attachment (core's, the CMS's, a Skyforms role
// that is not answer), or one of core's own links (the safety net). It is
// the line account erasure draws for a legacy Media (personalOnErasureSQL).
var legacyAnswerSharedSQL = `(EXISTS (
		SELECT 1 FROM media_attachments link
		WHERE link.media_id = media.id
		  AND NOT (link.owner_service = '` + string(authz.ProductForms) + `' AND link.role = '` + string(RoleFormsAnswer) + `')
	) OR EXISTS (` + coreLinksSQL("media.id") + `))`

// legacyAnswerMovableSQL holds for a Media the move may take: a current,
// public legacy Media whose detach expiry is not held, that no scan has
// claimed, and that only Skyforms answers use.
var legacyAnswerMovableSQL = `media.purpose = '` + PurposeLegacy + `' AND media.visibility = 'public'
	AND NOT media.detach_expiry_held AND media.scan_claim_id IS NULL AND ` + currentSQL + `
	AND NOT ` + legacyAnswerSharedSQL

// legacyAnswerMoveRecord is a row of media_legacy_answer_moves.
type legacyAnswerMoveRecord struct {
	publicKey     string
	publicDeleted bool
}

// legacyAnswerMove is the Media's move record; nil when it was never moved.
func (s *PostgresStore) legacyAnswerMove(ctx context.Context, id uuid.UUID) (*legacyAnswerMoveRecord, error) {
	var record legacyAnswerMoveRecord
	err := s.pool.QueryRow(ctx, `SELECT public_key, public_deleted_at IS NOT NULL FROM media_legacy_answer_moves WHERE media_id = $1`, id).
		Scan(&record.publicKey, &record.publicDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// legacyAnswerShared reports whether something other than a Skyforms
// answer uses the Media.
func (s *PostgresStore) legacyAnswerShared(ctx context.Context, id uuid.UUID) (bool, error) {
	var shared bool
	err := s.pool.QueryRow(ctx, `SELECT `+legacyAnswerSharedSQL+` FROM media WHERE media.id = $1`, id).Scan(&shared)
	return shared, err
}

// moveLegacyAnswer makes m, read at its public key, the private answer_file
// sealed holds, in one transaction: the Media row is locked and checked
// again (legacyAnswerMovableSQL, still at that key), takes the purpose, the
// private key and its encryption, waits for its scan with no expiry and no
// sizes or cover colours (as a private upload), the move is recorded, and
// each response gets its Media attachment (Skyforms', role answer; one that
// is there already is kept). moved is false, and nothing is written, when
// the Media no longer passes the check.
func (s *PostgresStore) moveLegacyAnswer(ctx context.Context, m Media, sealed SealedObject, responses []uuid.UUID, now time.Time) (moved bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM media WHERE id = $1 AND file_url = $2 AND `+legacyAnswerMovableSQL+` FOR UPDATE`,
		m.ID, m.Key).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE media SET
			purpose = $2, visibility = 'private',
			encryption_algorithm = $3, wrapped_data_key = $4, key_version = $5, file_url = $6,
			status = 'scanning', scan_result = NULL, scan_attempts = 0, scan_retry_at = NULL, expires_at = NULL,
			size_objects = '{}'::jsonb, cover_colors = '{}', cover_colors_computed = true, serving_policy_applied = true,
			updated_at = $7
		WHERE id = $1`,
		m.ID, PurposeAnswerFile, sealed.Encryption.Algorithm, sealed.Encryption.WrappedKey, sealed.Encryption.KeyVersion, sealed.Key, now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO media_legacy_answer_moves (media_id, public_key, moved_at) VALUES ($1, $2, $3)`,
		m.ID, m.Key, now); err != nil {
		return false, err
	}
	for _, response := range responses {
		if _, err := tx.Exec(ctx, `
			INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING`,
			m.ID, authz.ProductForms, legacyAnswerOwnerType, response.String(), RoleFormsAnswer); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// recordLegacyAnswerPublicDeleted records that a moved Media's public
// object is deleted; the time of the first deletion is kept.
func (s *PostgresStore) recordLegacyAnswerPublicDeleted(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE media_legacy_answer_moves SET public_deleted_at = COALESCE(public_deleted_at, GREATEST($2, moved_at)) WHERE media_id = $1`, id, at)
	return err
}
