package media

import (
	"context"
	"errors"
	"strings"

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
//
// A concurrent call may write the same link between the insert and the read
// that follows it, and a concurrent detach may remove it again before that
// read: the attach then tries again, so it answers the state after both
// rather than a missing row.
func (s *PostgresStore) Attach(ctx context.Context, a Attachment) (Attachment, bool, error) {
	for range 3 {
		created, err := scanAttachment(s.pool.QueryRow(ctx, `
			INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING
			RETURNING `+attachmentCols, a.MediaID, a.Owner.Service, a.Owner.Type, a.Owner.ID, a.Role))
		if err == nil {
			return created, true, nil
		}
		if _, refused := DatabaseLinkRefusal(err); refused || isForeignKeyViolation(err) {
			// No such Media (the foreign key), or it is archived, its purge
			// started, or its purpose no longer fits the role
			// (require_current_attached_media).
			return Attachment{}, false, ErrNotLinkable
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Attachment{}, false, err
		}
		existing, err := s.FindAttachment(ctx, a)
		if !errors.Is(err, ErrNotFound) {
			return existing, false, err
		}
	}
	return Attachment{}, false, errors.New("media: the link kept being written and removed under the attach")
}

// AttachHeld locks the held Media's row before anything else, so that two
// attaches of one held Media queue instead of upgrading a shared lock into a
// deadlock, then gives it back legacy when its purpose does not fit the
// role, and inserts the Media attachment.
func (s *PostgresStore) AttachHeld(ctx context.Context, a Attachment) (Attachment, bool, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Attachment{}, false, "", err
	}
	defer tx.Rollback(ctx)
	var purpose string
	err = tx.QueryRow(ctx, `SELECT purpose FROM media WHERE id = $1 AND detach_expiry_held AND `+currentSQL+` FOR UPDATE`, a.MediaID).Scan(&purpose)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, false, "", ErrNotLinkable
	}
	if err != nil {
		return Attachment{}, false, "", err
	}
	demotedFrom := ""
	if !fits(a.Owner.Service, a.Role, purpose) {
		if _, err := tx.Exec(ctx, `UPDATE media SET purpose = 'legacy' WHERE id = $1`, a.MediaID); err != nil {
			return Attachment{}, false, "", err
		}
		demotedFrom = purpose
	}
	created, err := scanAttachment(tx.QueryRow(ctx, `
		INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT ON CONSTRAINT media_attachments_link_key DO NOTHING
		RETURNING `+attachmentCols, a.MediaID, a.Owner.Service, a.Owner.Type, a.Owner.ID, a.Role))
	if errors.Is(err, pgx.ErrNoRows) {
		// The same link is there already; nothing changes.
		existing, err := scanAttachment(tx.QueryRow(ctx, `SELECT `+attachmentCols+` FROM media_attachments
			WHERE media_id = $1 AND owner_service = $2 AND owner_type = $3 AND owner_id = $4 AND role = $5`,
			a.MediaID, a.Owner.Service, a.Owner.Type, a.Owner.ID, a.Role))
		return existing, false, "", err
	}
	if _, refused := DatabaseLinkRefusal(err); refused || isForeignKeyViolation(err) {
		return Attachment{}, false, "", ErrNotLinkable
	}
	if err != nil {
		return Attachment{}, false, "", err
	}
	return created, true, demotedFrom, tx.Commit(ctx)
}

func (s *PostgresStore) HeldBy(ctx context.Context, mediaID uuid.UUID, product authz.Product) (bool, error) {
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM media_attachments WHERE media_id = $1 AND owner_service = $2)`,
		mediaID, product).Scan(&held)
	return held, err
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

// DatabaseLinkRefusal recognizes a link the database refused because the
// Media's purpose does not fit the role (require_current_attached_media,
// migration 20260926161000): the legacy backfill gave a legacy Media a
// purpose between the link's check and its write. A store returns it as the
// refusal the link check gives a Media that cannot be linked; retrying the
// link gets the ordinary purpose check. Any other error is not one.
func DatabaseLinkRefusal(err error) (*LinkRefusal, bool) {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" || pg.ConstraintName != "media_attachment_purpose_fits" {
		return nil, false
	}
	mediaID, role, _ := strings.Cut(pg.Detail, " ")
	id, parseErr := uuid.Parse(mediaID)
	if parseErr != nil {
		return nil, false
	}
	return &LinkRefusal{Err: ErrNotLinkable, MediaID: id, Role: Role(role)}, true
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
