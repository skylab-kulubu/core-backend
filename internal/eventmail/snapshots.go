package eventmail

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

const SnapshotRetention = 7 * 24 * time.Hour

const (
	snapshotSweepTimeout  = 30 * time.Second
	snapshotDeleteTimeout = 5 * time.Second
)

type Snapshot struct {
	MailListID uuid.UUID
	EventID    uuid.UUID
	ExpiresAt  time.Time
}

type SnapshotStore interface {
	Track(ctx context.Context, snapshot Snapshot) error
	Expired(ctx context.Context, before time.Time, limit int) ([]Snapshot, error)
	Forget(ctx context.Context, mailListID uuid.UUID) error
}

type PostgresSnapshotStore struct {
	pool *pgxpool.Pool
}

func NewPostgresSnapshotStore(pool *pgxpool.Pool) *PostgresSnapshotStore {
	return &PostgresSnapshotStore{pool: pool}
}

func (s *PostgresSnapshotStore) Track(ctx context.Context, snapshot Snapshot) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO event_mail_snapshots (mail_list_id, event_id, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (mail_list_id) DO UPDATE SET event_id = EXCLUDED.event_id, expires_at = EXCLUDED.expires_at`,
		snapshot.MailListID, snapshot.EventID, snapshot.ExpiresAt)
	return err
}

func (s *PostgresSnapshotStore) Expired(ctx context.Context, before time.Time, limit int) ([]Snapshot, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT mail_list_id, event_id, expires_at
		FROM event_mail_snapshots
		WHERE expires_at <= $1
		ORDER BY expires_at, mail_list_id
		LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Snapshot, 0)
	for rows.Next() {
		var snapshot Snapshot
		if err := rows.Scan(&snapshot.MailListID, &snapshot.EventID, &snapshot.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}

func (s *PostgresSnapshotStore) Forget(ctx context.Context, mailListID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM event_mail_snapshots WHERE mail_list_id = $1`, mailListID)
	return err
}

type MemorySnapshotStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]Snapshot
}

func NewMemorySnapshotStore() *MemorySnapshotStore {
	return &MemorySnapshotStore{rows: map[uuid.UUID]Snapshot{}}
}

func (s *MemorySnapshotStore) Track(_ context.Context, snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[snapshot.MailListID] = snapshot
	return nil
}

func (s *MemorySnapshotStore) Expired(_ context.Context, before time.Time, limit int) ([]Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Snapshot, 0)
	for _, snapshot := range s.rows {
		if !snapshot.ExpiresAt.After(before) {
			out = append(out, snapshot)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *MemorySnapshotStore) Forget(_ context.Context, mailListID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, mailListID)
	return nil
}

func PruneSnapshots(ctx context.Context, snapshots SnapshotStore, lists mail.Lists, now time.Time, limit int) error {
	rows, err := snapshots.Expired(ctx, now, limit)
	if err != nil {
		return err
	}
	var errs []error
	for _, snapshot := range rows {
		deleteCtx, cancel := context.WithTimeout(ctx, snapshotDeleteTimeout)
		err := lists.DeleteList(deleteCtx, snapshot.MailListID)
		cancel()
		if err != nil && !errors.Is(err, mail.ErrListNotFound) {
			errs = append(errs, err)
			continue
		}
		if err := snapshots.Forget(ctx, snapshot.MailListID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func MaintainSnapshotRetention(ctx context.Context, snapshots SnapshotStore, lists mail.Lists, interval time.Duration, report func(error)) {
	prune := func() {
		sweepCtx, cancel := context.WithTimeout(ctx, snapshotSweepTimeout)
		defer cancel()
		if err := PruneSnapshots(sweepCtx, snapshots, lists, time.Now().UTC(), 1000); err != nil && report != nil {
			report(err)
		}
	}
	go func() {
		prune()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
}
