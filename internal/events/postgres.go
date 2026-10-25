package events

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store records consumed events.
type Store interface {
	// Consume records (consumer, source, id) and runs fn in the same
	// transaction, which commits when fn returns nil. duplicate is true,
	// and fn is not run, when the event was recorded before. When fn
	// fails the transaction rolls back, the record with it, and its error
	// is returned.
	Consume(ctx context.Context, consumer, source, id string, fn func(pgx.Tx) error) (duplicate bool, err error)
	// DeleteBefore deletes the records made before cutoff and says how
	// many.
	DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// Postgres is Store on consumed_events (migration 20261021153000).
type Postgres struct {
	pool *pgxpool.Pool
	// batch bounds one DELETE of DeleteBefore.
	batch int
}

// NewPostgres builds the store.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool, batch: 1000}
}

// Consume claims the event with an INSERT the primary key keeps unique: a
// second claim of the same event, on this replica or another, waits for the
// first transaction and then finds the row (or takes it, when the first
// rolled back).
func (p *Postgres) Consume(ctx context.Context, consumer, source, id string, fn func(pgx.Tx) error) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("events: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `
		INSERT INTO consumed_events (consumer, source, event_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (consumer, source, event_id) DO NOTHING
	`, consumer, source, id)
	if err != nil {
		return false, fmt.Errorf("events: record: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return true, nil
	}
	if err := fn(tx); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("events: commit: %w", err)
	}
	return false, nil
}

// DeleteBefore deletes in batches, so no statement holds many rows' locks.
func (p *Postgres) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for {
		tag, err := p.pool.Exec(ctx, `
			DELETE FROM consumed_events
			WHERE ctid IN (
				SELECT ctid FROM consumed_events
				WHERE processed_at < $1
				LIMIT $2
			)
		`, cutoff, p.batch)
		if err != nil {
			return total, fmt.Errorf("events: delete consumed records: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(p.batch) {
			return total, nil
		}
	}
}
