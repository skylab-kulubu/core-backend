package media

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

const attachmentCols = `id, media_id, owner_service, owner_type, owner_id, role, created_at`

func scanAttachment(row rowScanner) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.MediaID, &a.Owner.Service, &a.Owner.Type, &a.Owner.ID, &a.Role, &a.CreatedAt)
	return a, err
}

// Attach inserts the Media attachment; the database's triggers refuse a Media
// that is not current and make the Media attached. The same link again
// returns the row already there.
func (s *PostgresStore) Attach(ctx context.Context, a Attachment) (Attachment, bool, error) {
	created, err := scanAttachment(s.pool.QueryRow(ctx, `
		INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING
		RETURNING `+attachmentCols, a.MediaID, a.Owner.Service, a.Owner.Type, a.Owner.ID, a.Role))
	if err == nil {
		return created, true, nil
	}
	if isForeignKeyViolation(err) {
		// No such Media (the foreign key), or it is archived or its purge
		// started (require_current_attached_media).
		return Attachment{}, false, ErrNotLinkable
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, false, err
	}
	// The same link, written by a concurrent call.
	existing, err := s.FindAttachment(ctx, a)
	return existing, false, err
}

func (s *PostgresStore) FindAttachment(ctx context.Context, a Attachment) (Attachment, error) {
	existing, err := scanAttachment(s.pool.QueryRow(ctx, `SELECT `+attachmentCols+` FROM media_attachments
		WHERE media_id = $1 AND owner_service = $2 AND owner_type = $3 AND owner_id = $4 AND role = $5`,
		a.MediaID, a.Owner.Service, a.Owner.Type, a.Owner.ID, a.Role))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	return existing, err
}

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

func (s *PostgresStore) GetAttachment(ctx context.Context, mediaID, attachmentID uuid.UUID) (Attachment, error) {
	a, err := scanAttachment(s.pool.QueryRow(ctx, `SELECT `+attachmentCols+` FROM media_attachments
		WHERE id = $1 AND media_id = $2`, attachmentID, mediaID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	return a, err
}

// Detach deletes the Media attachment; the database's status trigger detaches
// the Media when it was the last.
func (s *PostgresStore) Detach(ctx context.Context, mediaID, attachmentID uuid.UUID, service authz.Product) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM media_attachments WHERE id = $1 AND media_id = $2 AND owner_service = $3`,
		attachmentID, mediaID, service)
	return err
}
