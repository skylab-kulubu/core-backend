package competitor

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const competitorCols = `id, user_id, event_id, score, is_winner, withdrawn_at, withdrawn_by, created_at, updated_at`

func (s *PostgresStore) List(ctx context.Context) ([]Competitor, error) {
	return s.query(ctx, `SELECT `+competitorCols+` FROM competitors WHERE withdrawn_at IS NULL ORDER BY created_at`)
}

func (s *PostgresStore) ListLifecycle(ctx context.Context, visibility lifecycle.Visibility) ([]Competitor, error) {
	q := `SELECT ` + competitorCols + ` FROM competitors`
	if condition := visibility.SQLCondition("withdrawn_at"); condition != "" {
		q += ` WHERE ` + condition
	}
	return s.query(ctx, q+` ORDER BY created_at`)
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Competitor, error) {
	return s.get(ctx, id, false)
}

func (s *PostgresStore) GetIncludingWithdrawn(ctx context.Context, id uuid.UUID) (Competitor, error) {
	return s.get(ctx, id, true)
}

func (s *PostgresStore) get(ctx context.Context, id uuid.UUID, includeWithdrawn bool) (Competitor, error) {
	q := `SELECT ` + competitorCols + ` FROM competitors WHERE id = $1`
	if !includeWithdrawn {
		q += ` AND withdrawn_at IS NULL`
	}
	c, err := scanCompetitor(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Competitor{}, ErrNotFound
	}
	return c, err
}

func (s *PostgresStore) Create(ctx context.Context, c Competitor) (Competitor, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	return scanCompetitor(s.pool.QueryRow(ctx, `
		INSERT INTO competitors (id, user_id, event_id, score, is_winner)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+competitorCols, c.ID, c.UserID, c.EventID, c.Score, c.IsWinner))
}

func (s *PostgresStore) Update(ctx context.Context, c Competitor) (Competitor, error) {
	got, err := scanCompetitor(s.pool.QueryRow(ctx, `
		UPDATE competitors SET
			user_id = $2, event_id = $3, score = $4, is_winner = $5, updated_at = now()
		WHERE id = $1 AND withdrawn_at IS NULL
		RETURNING `+competitorCols, c.ID, c.UserID, c.EventID, c.Score, c.IsWinner))
	if errors.Is(err, pgx.ErrNoRows) {
		return Competitor{}, ErrNotFound
	}
	return got, err
}

func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM competitors WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Competitor, error) {
	return s.query(ctx, `SELECT `+competitorCols+` FROM competitors WHERE event_id = $1 AND withdrawn_at IS NULL ORDER BY created_at`, eventID)
}

func (s *PostgresStore) ListByUser(ctx context.Context, userID uuid.UUID) ([]Competitor, error) {
	return s.query(ctx, `SELECT `+competitorCols+` FROM competitors WHERE user_id = $1 AND withdrawn_at IS NULL ORDER BY created_at`, userID)
}

func (s *PostgresStore) ListByOwnerTeam(ctx context.Context, ownerTeam string) ([]Competitor, error) {
	return s.query(ctx, `
		SELECT c.id, c.user_id, c.event_id, c.score, c.is_winner, c.withdrawn_at, c.withdrawn_by, c.created_at, c.updated_at
		FROM competitors c
		JOIN events e ON e.id = c.event_id
		WHERE e.owner_team = $1 AND e.archived_at IS NULL AND c.withdrawn_at IS NULL
		ORDER BY c.created_at`, ownerTeam)
}

func (s *PostgresStore) ExistsUserEvent(ctx context.Context, userID, eventID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1) FROM competitors WHERE user_id = $1 AND event_id = $2
	`, userID, eventID).Scan(&n)
	return n > 0, err
}

func (s *PostgresStore) Winner(ctx context.Context, eventID uuid.UUID) (Competitor, error) {
	c, err := scanCompetitor(s.pool.QueryRow(ctx, `
		SELECT `+competitorCols+` FROM competitors WHERE event_id = $1 AND is_winner = true AND withdrawn_at IS NULL LIMIT 1
	`, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Competitor{}, ErrNotFound
	}
	return c, err
}

func (s *PostgresStore) query(ctx context.Context, q string, args ...any) ([]Competitor, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Competitor, 0)
	for rows.Next() {
		c, err := scanCompetitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCompetitor(row rowScanner) (Competitor, error) {
	var c Competitor
	var created, updated time.Time
	err := row.Scan(&c.ID, &c.UserID, &c.EventID, &c.Score, &c.IsWinner, &c.WithdrawnAt, &c.WithdrawnBy, &created, &updated)
	c.CreatedAt = created
	c.UpdatedAt = updated
	return c, err
}
