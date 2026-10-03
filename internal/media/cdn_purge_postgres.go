package media

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The CDN purge queue (CDNPurgeQueue) in media_cdn_purges.

// cdnPurgeQueueConns are the queue's own connections.
const cdnPurgeQueueConns = 2

// NewCDNPurgeQueuePool opens the purge queue's own connections to the
// database at connString; core gives the queue a PostgresStore over it.
// The queue must not share core's pool: a blob purge holds one of its
// connections in a transaction whose reference check takes SHARE locks
// (on users, events, …) while it deletes the object, and the delete queues
// the address. Requests waiting on those locks can hold every other
// connection of a shared pool, and the queue's insert would then wait for
// one until the purge's timeout, with the locks held all the while. Two
// connections of its own never wait on core's.
func NewCDNPurgeQueuePool(ctx context.Context, connString string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		// pgx's error may quote the address, password and all.
		return nil, errors.New("media: CDN purge queue: the database address cannot be read")
	}
	config.MaxConns = cdnPurgeQueueConns
	config.MinConns = 0
	return pgxpool.NewWithConfig(ctx, config)
}

func (s *PostgresStore) EnqueueCDNPurges(ctx context.Context, urls []string, now time.Time) error {
	if len(urls) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO media_cdn_purges (url, queued_at, next_attempt_at, attempts)
		SELECT url, $2, $2, 0 FROM unnest($1::text[]) AS url
		ON CONFLICT (url) DO UPDATE SET queued_at = EXCLUDED.queued_at, next_attempt_at = EXCLUDED.next_attempt_at, attempts = 0`,
		urls, now)
	return err
}

func (s *PostgresStore) ClaimCDNPurges(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]CDNPurgeEntry, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE media_cdn_purges SET next_attempt_at = $2
		WHERE url IN (
			SELECT url FROM media_cdn_purges
			WHERE next_attempt_at <= $1
			ORDER BY next_attempt_at, url
			LIMIT $3
			FOR UPDATE SKIP LOCKED)
		RETURNING url, queued_at, attempts`,
		now, now.Add(lease), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CDNPurgeEntry
	for rows.Next() {
		var e CDNPurgeEntry
		if err := rows.Scan(&e.URL, &e.QueuedAt, &e.Attempts); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CompleteCDNPurges(ctx context.Context, done []CDNPurgeEntry) error {
	if len(done) == 0 {
		return nil
	}
	urls, queued := make([]string, len(done)), make([]time.Time, len(done))
	for i, e := range done {
		urls[i], queued[i] = e.URL, e.QueuedAt
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM media_cdn_purges p
		USING unnest($1::text[], $2::timestamptz[]) AS d(url, queued_at)
		WHERE p.url = d.url AND p.queued_at = d.queued_at`,
		urls, queued)
	return err
}

func (s *PostgresStore) RetryCDNPurges(ctx context.Context, failed []CDNPurgeEntry, at []time.Time) error {
	if len(failed) == 0 {
		return nil
	}
	urls, queued := make([]string, len(failed)), make([]time.Time, len(failed))
	for i, e := range failed {
		urls[i], queued[i] = e.URL, e.QueuedAt
	}
	// An address queued again meanwhile is due now, its failures
	// forgotten; it is left so.
	_, err := s.pool.Exec(ctx, `
		UPDATE media_cdn_purges p SET attempts = p.attempts + 1, next_attempt_at = f.at
		FROM unnest($1::text[], $2::timestamptz[], $3::timestamptz[]) AS f(url, queued_at, at)
		WHERE p.url = f.url AND p.queued_at = f.queued_at`,
		urls, queued, at)
	return err
}

func (s *PostgresStore) DropCDNPurges(ctx context.Context, queuedBefore time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM media_cdn_purges WHERE queued_at < $1`, queuedBefore)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *PostgresStore) CDNPurgeBacklog(ctx context.Context, now time.Time) (int, time.Duration, error) {
	var count int
	var oldest *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT count(*), min(queued_at) FROM media_cdn_purges`).Scan(&count, &oldest); err != nil {
		return 0, 0, err
	}
	if oldest == nil {
		return 0, 0, nil
	}
	return count, max(now.Sub(*oldest), 0), nil
}

var _ CDNPurgeQueue = (*PostgresStore)(nil)
