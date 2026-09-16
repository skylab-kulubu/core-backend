package season

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const seasonCols = `id, name, start_date, end_date, active, created_at, updated_at`

func (s *PostgresStore) List(ctx context.Context, activeOnly bool) ([]Season, error) {
	q := `SELECT ` + seasonCols + ` FROM seasons`
	if activeOnly {
		q += ` WHERE active = true`
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
	item, err := scanSeason(s.pool.QueryRow(ctx, `SELECT `+seasonCols+` FROM seasons WHERE id = $1`, id))
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
		WHERE id = $1
		RETURNING `+seasonCols, in.ID, in.Name, in.StartDate, in.EndDate, in.Active))
	if errors.Is(err, pgx.ErrNoRows) {
		return Season{}, ErrNotFound
	}
	return item, err
}

func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM seasons WHERE id = $1`, id)
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
	err := row.Scan(&item.ID, &item.Name, &item.StartDate, &item.EndDate, &item.Active, &item.CreatedAt, &item.UpdatedAt)
	return item, err
}
