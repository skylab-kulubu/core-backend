package skypass

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

// WalletPass is one Google Wallet pass core issued: a row of
// skypass_google_wallet_passes. It holds no secret: the TOTP secret is
// derived from the Wallet key and PassID.
type WalletPass struct {
	PassID string
	UserID uuid.UUID
	// LastCounter is the highest TOTP step a check-in accepted; a code of
	// that step or an older one is not taken again.
	LastCounter int64
	CreatedAt   time.Time
	// RevokedAt is set once the pass stopped opening the door: its codes
	// are refused while its Google object waits to be withdrawn.
	RevokedAt *time.Time
}

// WalletStore keeps the passes.
type WalletStore interface {
	// ActivePass is the person's pass that is not revoked.
	ActivePass(ctx context.Context, userID uuid.UUID) (WalletPass, bool, error)
	// CreatePass adds a pass, or returns the person's active one when there
	// is one already. A person who is not active gets ErrNotFound.
	CreatePass(ctx context.Context, userID uuid.UUID, passID string, at time.Time) (WalletPass, error)
	PassByID(ctx context.Context, passID string) (WalletPass, bool, error)
	// ConsumeCounter records counter as used when the pass is active and
	// counter is above its LastCounter, and reports whether it did.
	ConsumeCounter(ctx context.Context, passID string, counter int64) (bool, error)
	Revoke(ctx context.Context, passID string, at time.Time) error
	// PassesOf lists every pass of the person, revoked ones included.
	PassesOf(ctx context.Context, userID uuid.UUID) ([]WalletPass, error)
	Delete(ctx context.Context, passID string) error
}

// MemoryWalletStore is a WalletStore in memory, for tests (core does not
// start without DATABASE_URL).
type MemoryWalletStore struct {
	mu     sync.Mutex
	passes map[string]WalletPass
}

func NewMemoryWalletStore() *MemoryWalletStore {
	return &MemoryWalletStore{passes: map[string]WalletPass{}}
}

func (s *MemoryWalletStore) ActivePass(_ context.Context, userID uuid.UUID) (WalletPass, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.passes {
		if p.UserID == userID && p.RevokedAt == nil {
			return p, true, nil
		}
	}
	return WalletPass{}, false, nil
}

func (s *MemoryWalletStore) CreatePass(_ context.Context, userID uuid.UUID, passID string, at time.Time) (WalletPass, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.passes {
		if p.UserID == userID && p.RevokedAt == nil {
			return p, nil
		}
	}
	if _, taken := s.passes[passID]; taken {
		return WalletPass{}, ErrConflict
	}
	p := WalletPass{PassID: passID, UserID: userID, CreatedAt: at}
	s.passes[passID] = p
	return p, nil
}

func (s *MemoryWalletStore) PassByID(_ context.Context, passID string) (WalletPass, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.passes[passID]
	return p, ok, nil
}

func (s *MemoryWalletStore) ConsumeCounter(_ context.Context, passID string, counter int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.passes[passID]
	if !ok || p.RevokedAt != nil || counter <= p.LastCounter {
		return false, nil
	}
	p.LastCounter = counter
	s.passes[passID] = p
	return true, nil
}

func (s *MemoryWalletStore) Revoke(_ context.Context, passID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.passes[passID]
	if ok && p.RevokedAt == nil {
		p.RevokedAt = &at
		s.passes[passID] = p
	}
	return nil
}

func (s *MemoryWalletStore) PassesOf(_ context.Context, userID uuid.UUID) ([]WalletPass, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []WalletPass
	for _, p := range s.passes {
		if p.UserID == userID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryWalletStore) Delete(_ context.Context, passID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.passes, passID)
	return nil
}

// PostgresWalletStore keeps the passes in skypass_google_wallet_passes.
type PostgresWalletStore struct {
	pool *pgxpool.Pool
}

func NewPostgresWalletStore(pool *pgxpool.Pool) *PostgresWalletStore {
	return &PostgresWalletStore{pool: pool}
}

const walletPassColumns = `pass_id, user_id, last_counter, created_at, revoked_at`

func scanWalletPass(row pgx.Row) (WalletPass, error) {
	var p WalletPass
	err := row.Scan(&p.PassID, &p.UserID, &p.LastCounter, &p.CreatedAt, &p.RevokedAt)
	return p, err
}

func (s *PostgresWalletStore) ActivePass(ctx context.Context, userID uuid.UUID) (WalletPass, bool, error) {
	p, err := scanWalletPass(s.pool.QueryRow(ctx,
		`SELECT `+walletPassColumns+` FROM skypass_google_wallet_passes WHERE user_id = $1 AND revoked_at IS NULL`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return WalletPass{}, false, nil
	}
	return p, err == nil, err
}

func (s *PostgresWalletStore) CreatePass(ctx context.Context, userID uuid.UUID, passID string, at time.Time) (WalletPass, error) {
	p, err := scanWalletPass(s.pool.QueryRow(ctx, `
		INSERT INTO skypass_google_wallet_passes (pass_id, user_id, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) WHERE revoked_at IS NULL DO NOTHING
		RETURNING `+walletPassColumns, passID, userID, at))
	if subjectlock.IsInactiveAccountReference(err) {
		return WalletPass{}, ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Another request made the person's pass first.
		existing, ok, err := s.ActivePass(ctx, userID)
		if err != nil {
			return WalletPass{}, err
		}
		if !ok {
			return WalletPass{}, ErrConflict
		}
		return existing, nil
	}
	return p, err
}

func (s *PostgresWalletStore) PassByID(ctx context.Context, passID string) (WalletPass, bool, error) {
	p, err := scanWalletPass(s.pool.QueryRow(ctx,
		`SELECT `+walletPassColumns+` FROM skypass_google_wallet_passes WHERE pass_id = $1`, passID))
	if errors.Is(err, pgx.ErrNoRows) {
		return WalletPass{}, false, nil
	}
	return p, err == nil, err
}

func (s *PostgresWalletStore) ConsumeCounter(ctx context.Context, passID string, counter int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE skypass_google_wallet_passes SET last_counter = $2
		WHERE pass_id = $1 AND revoked_at IS NULL AND last_counter < $2`, passID, counter)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PostgresWalletStore) Revoke(ctx context.Context, passID string, at time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE skypass_google_wallet_passes SET revoked_at = $2 WHERE pass_id = $1 AND revoked_at IS NULL`, passID, at)
	return err
}

func (s *PostgresWalletStore) PassesOf(ctx context.Context, userID uuid.UUID) ([]WalletPass, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+walletPassColumns+` FROM skypass_google_wallet_passes WHERE user_id = $1 ORDER BY created_at, pass_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WalletPass
	for rows.Next() {
		p, err := scanWalletPass(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PostgresWalletStore) Delete(ctx context.Context, passID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM skypass_google_wallet_passes WHERE pass_id = $1`, passID)
	return err
}
