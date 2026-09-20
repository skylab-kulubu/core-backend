package season

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const seasonCols = `id, name, start_date, end_date, active, archived_at, archived_by, created_at, updated_at`

func (s *PostgresStore) List(ctx context.Context, activeOnly bool) ([]Season, error) {
	return s.list(ctx, activeOnly, lifecycle.CurrentOnly)
}

func (s *PostgresStore) ListLifecycle(ctx context.Context, visibility lifecycle.Visibility) ([]Season, error) {
	return s.list(ctx, false, visibility)
}

func (s *PostgresStore) list(ctx context.Context, activeOnly bool, visibility lifecycle.Visibility) ([]Season, error) {
	q := `SELECT ` + seasonCols + ` FROM seasons`
	where := make([]string, 0, 2)
	if condition := visibility.SQLCondition("archived_at"); condition != "" {
		where = append(where, condition)
	}
	if activeOnly {
		where = append(where, `active = true`)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY start_date NULLS LAST, name`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Season, 0)
	for rows.Next() {
		item, err := scanSeason(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Season, error) {
	return s.get(ctx, id, false)
}

func (s *PostgresStore) GetIncludingArchived(ctx context.Context, id uuid.UUID) (Season, error) {
	return s.get(ctx, id, true)
}

func (s *PostgresStore) get(ctx context.Context, id uuid.UUID, includeArchived bool) (Season, error) {
	q := `SELECT ` + seasonCols + ` FROM seasons WHERE id = $1`
	if !includeArchived {
		q += ` AND archived_at IS NULL`
	}
	item, err := scanSeason(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Season{}, ErrNotFound
	}
	return item, err
}

func (s *PostgresStore) Create(ctx context.Context, in Season) (Season, error) {
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	return scanSeason(s.pool.QueryRow(ctx, `
		INSERT INTO seasons (id, name, start_date, end_date, active)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING `+seasonCols, in.ID, in.Name, in.StartDate, in.EndDate, in.Active))
}

func (s *PostgresStore) Update(ctx context.Context, in Season) (Season, error) {
	item, err := scanSeason(s.pool.QueryRow(ctx, `
		UPDATE seasons SET name = $2, start_date = $3, end_date = $4, active = $5, updated_at = now()
		WHERE id = $1 AND archived_at IS NULL
		RETURNING `+seasonCols, in.ID, in.Name, in.StartDate, in.EndDate, in.Active))
	if errors.Is(err, pgx.ErrNoRows) {
		return Season{}, ErrNotFound
	}
	return item, err
}

func (s *PostgresStore) Archive(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE seasons
		SET archived_at = COALESCE(archived_at, now()),
			archived_by = CASE WHEN archived_at IS NULL THEN $2 ELSE archived_by END,
			updated_at = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id, actorID)
	if err != nil {
		if subjectlock.IsInactiveAccountReference(err) {
			return ErrForbidden
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Restore(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE seasons
		SET archived_at = NULL,
			archived_by = NULL,
			updated_at = CASE WHEN archived_at IS NOT NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSeason(row rowScanner) (Season, error) {
	var item Season
	err := row.Scan(&item.ID, &item.Name, &item.StartDate, &item.EndDate, &item.Active, &item.ArchivedAt, &item.ArchivedBy, &item.CreatedAt, &item.UpdatedAt)
	return item, err
}
