package consoleaudit

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockSQL and unlockSQL are the reader's session advisory lock: one core
// reads at a time.
const (
	lockSQL   = `SELECT pg_try_advisory_lock(hashtextextended('core-backend console audit', 0))`
	unlockSQL = `SELECT pg_advisory_unlock(hashtextextended('core-backend console audit', 0))`
)

// PostgresStore keeps the cursor in console_audit_cursor.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (p *PostgresStore) Locked(ctx context.Context, fn func(ctx context.Context) error) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var held bool
	if err := conn.QueryRow(ctx, lockSQL).Scan(&held); err != nil {
		return err
	}
	if !held {
		return ErrLocked
	}
	defer func() {
		// The connection may be gone; its session lock goes with it.
		if _, err := conn.Exec(context.WithoutCancel(ctx), unlockSQL); err != nil {
			conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()
	return fn(ctx)
}

func (p *PostgresStore) Load(ctx context.Context) (Cursor, bool, error) {
	var c Cursor
	err := p.pool.QueryRow(ctx, `SELECT event_time, event_ids FROM console_audit_cursor WHERE id = 1`).Scan(&c.Time, &c.IDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cursor{}, false, nil
	}
	if err != nil {
		return Cursor{}, false, err
	}
	return c, true, nil
}

func (p *PostgresStore) Save(ctx context.Context, c Cursor) error {
	ids := c.IDs
	if ids == nil {
		ids = []string{}
	}
	_, err := p.pool.Exec(context.WithoutCancel(ctx), `
		INSERT INTO console_audit_cursor (id, event_time, event_ids, updated_at) VALUES (1, $1, $2, now())
		ON CONFLICT (id) DO UPDATE SET event_time = $1, event_ids = $2, updated_at = now()`, c.Time, ids)
	return err
}

// MemoryStore is a Store in memory, for tests.
type MemoryStore struct {
	mu     sync.Mutex
	cursor *Cursor
	locked bool
}

func (m *MemoryStore) Locked(ctx context.Context, fn func(ctx context.Context) error) error {
	m.mu.Lock()
	if m.locked {
		m.mu.Unlock()
		return ErrLocked
	}
	m.locked = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.locked = false
		m.mu.Unlock()
	}()
	return fn(ctx)
}

func (m *MemoryStore) Load(context.Context) (Cursor, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cursor == nil {
		return Cursor{}, false, nil
	}
	return Cursor{Time: m.cursor.Time, IDs: slices.Clone(m.cursor.IDs)}, true, nil
}

func (m *MemoryStore) Save(_ context.Context, c Cursor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor = &Cursor{Time: c.Time, IDs: slices.Clone(c.IDs)}
	return nil
}
