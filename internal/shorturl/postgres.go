package shorturl

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const urlCols = `id, alias, url, click_count, created_by, created_at, updated_at`

func (s *PostgresStore) Create(ctx context.Context, u URL) (URL, error) {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	got, err := scanURL(s.pool.QueryRow(ctx, `
		INSERT INTO urls (id, alias, url, click_count, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+urlCols, u.ID, u.Alias, u.URL, u.ClickCount, u.CreatedBy))
	return got, mapURLErr(err)
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (URL, error) {
	u, err := scanURL(s.pool.QueryRow(ctx, `SELECT `+urlCols+` FROM urls WHERE id = $1`, id))
	return u, mapURLErr(err)
}

func (s *PostgresStore) GetByAlias(ctx context.Context, alias string) (URL, error) {
	u, err := scanURL(s.pool.QueryRow(ctx, `SELECT `+urlCols+` FROM urls WHERE alias = $1`, alias))
	return u, mapURLErr(err)
}

func (s *PostgresStore) ListByCreator(ctx context.Context, userID uuid.UUID) ([]URL, error) {
	return s.list(ctx, `SELECT `+urlCols+` FROM urls WHERE created_by = $1 ORDER BY created_at DESC`, userID)
}

func (s *PostgresStore) ListAll(ctx context.Context) ([]URL, error) {
	return s.list(ctx, `SELECT `+urlCols+` FROM urls ORDER BY created_at DESC`)
}

func (s *PostgresStore) Update(ctx context.Context, u URL) (URL, error) {
	got, err := scanURL(s.pool.QueryRow(ctx, `
		UPDATE urls SET alias = $2, url = $3, updated_at = now()
		WHERE id = $1
		RETURNING `+urlCols, u.ID, u.Alias, u.URL))
	return got, mapURLErr(err)
}

func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM urls WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) IncrementClicks(ctx context.Context, id uuid.UUID) (URL, error) {
	u, err := scanURL(s.pool.QueryRow(ctx, `
		UPDATE urls SET click_count = click_count + 1, updated_at = now()
		WHERE id = $1
		RETURNING `+urlCols, id))
	return u, mapURLErr(err)
}

func (s *PostgresStore) list(ctx context.Context, q string, args ...any) ([]URL, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]URL, 0)
	for rows.Next() {
		u, err := scanURL(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

type urlRow interface {
	Scan(dest ...any) error
}

func scanURL(row urlRow) (URL, error) {
	var u URL
	var created, updated time.Time
	err := row.Scan(&u.ID, &u.Alias, &u.URL, &u.ClickCount, &u.CreatedBy, &created, &updated)
	u.CreatedAt = created
	u.UpdatedAt = updated
	return u, err
}

func mapURLErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}
