package formsfeed

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kinds of record whose version core keeps (forms_record_versions).
const (
	KindResponse = "response"
	KindForm     = "form"
)

// VersionRetention is how long a record's version is kept after its last
// change: as long as consumed_events keeps an event (ADR-0071 §6), longer
// than an outbox (7 days) or a DLQ (14 days) can hand an old event over.
const VersionRetention = 30 * 24 * time.Hour

// Versions keeps the latest version core handled of each record.
type Versions interface {
	// Advance records version as the record's latest in tx and answers true
	// when it is newer than the one kept (or, with orEqual, the same); it
	// answers false, recording nothing, for an older one. The row stays
	// locked until tx ends, so two deliveries of one record are handled one
	// after the other.
	Advance(ctx context.Context, tx pgx.Tx, kind string, id uuid.UUID, version int64, orEqual bool) (bool, error)
}

// PostgresVersions is Versions on forms_record_versions.
type PostgresVersions struct{}

func (PostgresVersions) Advance(ctx context.Context, tx pgx.Tx, kind string, id uuid.UUID, version int64, orEqual bool) (bool, error) {
	if tx == nil {
		return false, errors.New("formsfeed: a version needs a transaction")
	}
	cmp := "<"
	if orEqual {
		cmp = "<="
	}
	var kept int64
	err := tx.QueryRow(ctx, `
		INSERT INTO forms_record_versions AS v (kind, record_id, version, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (kind, record_id) DO UPDATE SET version = EXCLUDED.version, updated_at = EXCLUDED.updated_at
		WHERE v.version `+cmp+` EXCLUDED.version
		RETURNING version`, kind, id, version).Scan(&kept)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// DeleteOldVersions deletes the versions last changed before before, 1,000
// rows a statement, and counts them.
func DeleteOldVersions(ctx context.Context, pool *pgxpool.Pool, before time.Time) (int64, error) {
	var total int64
	for {
		tag, err := pool.Exec(ctx, `
			DELETE FROM forms_record_versions WHERE ctid IN (
				SELECT ctid FROM forms_record_versions WHERE updated_at < $1 LIMIT 1000)`, before)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 1000 {
			return total, nil
		}
	}
}

// RunVersionCleanup deletes old versions at once and then hourly, until ctx
// ends; the channel closes when it has stopped. It runs whether or not the
// consumer is on, as consumed_events' cleanup does: versions left from a time
// it was on still go on time.
func RunVersionCleanup(ctx context.Context, pool *pgxpool.Pool, logf func(string, ...any)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			if _, err := DeleteOldVersions(ctx, pool, time.Now().Add(-VersionRetention)); err != nil && ctx.Err() == nil && logf != nil {
				logf(`{"event":"forms_record_versions_cleanup_failed","level":"warn"}`)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
