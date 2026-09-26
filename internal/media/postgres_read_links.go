package media

import (
	"context"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

func (s *PostgresStore) RecordReadLink(ctx context.Context, link ReadLinkRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO media_read_links (id, media_id, product, on_behalf_of, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		link.ID, link.MediaID, string(link.Product), link.OnBehalfOf, link.IssuedAt, link.ExpiresAt)
	if subjectlock.IsInactiveAccountReference(err) {
		return ErrLinkSubjectInactive
	}
	return err
}

func (s *PostgresStore) RecordReadLinkOpen(ctx context.Context, open ReadLinkOpen) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO media_read_link_opens (link_id, opened_at, client_ip) VALUES ($1, $2, $3)`,
		open.LinkID, open.OpenedAt, open.ClientIP)
	return err
}

// readLinkPruneBatch is how many rows one statement of the retention
// cleanup deletes.
const readLinkPruneBatch = 500

// PruneReadLinks deletes, a batch at a time, the read links issued before
// the cutoff, with their opens, and any open made before it. It reports the
// links it deleted.
func (s *PostgresStore) PruneReadLinks(ctx context.Context, before time.Time) (int64, error) {
	var links int64
	for i, statement := range []string{
		// An open goes with its link (ON DELETE CASCADE).
		`DELETE FROM media_read_links WHERE id IN (
			SELECT id FROM media_read_links WHERE issued_at < $1 ORDER BY issued_at LIMIT $2)`,
		`DELETE FROM media_read_link_opens WHERE ctid IN (
			SELECT ctid FROM media_read_link_opens WHERE opened_at < $1 ORDER BY opened_at LIMIT $2)`,
	} {
		for {
			tag, err := s.pool.Exec(ctx, statement, before, readLinkPruneBatch)
			if err != nil {
				return links, err
			}
			if i == 0 {
				links += tag.RowsAffected()
			}
			if tag.RowsAffected() < readLinkPruneBatch {
				break
			}
		}
	}
	return links, nil
}
