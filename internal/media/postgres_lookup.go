package media

import (
	"context"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// lookUpKeysSQL reads every Media of a batch of lookup keys ($1) through
// media_lookup_key_idx (migration 20260929180000), with whether the product
// ($2) holds a Media attachment to each: one query. A key is one Media's;
// were two to share one, the current and newest comes first.
const lookUpKeysSQL = `SELECT ` + mediaCols + `, media_lookup_key(file_url),
		EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id AND a.owner_service = $2)
	FROM media
	WHERE media_lookup_key(file_url) = ANY($1::text[])
	ORDER BY media_lookup_key(file_url), deleted_at IS NULL DESC, created_at DESC, id`

func (s *PostgresStore) LookUpKeys(ctx context.Context, keys []string, product authz.Product) ([]KeyMatch, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, lookUpKeysSQL, keys, product)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]KeyMatch, 0, len(keys))
	for rows.Next() {
		var match KeyMatch
		m, err := scanMedia(scanWith{rows, []any{&match.LookupKey, &match.Held}})
		if err != nil {
			return nil, err
		}
		match.Media = m
		out = append(out, match)
	}
	return out, rows.Err()
}

// scanWith scans a row of the media columns followed by more columns into
// extra.
type scanWith struct {
	row   rowScanner
	extra []any
}

func (s scanWith) Scan(dest ...any) error {
	return s.row.Scan(append(dest, s.extra...)...)
}
