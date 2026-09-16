package media

import (
	"context"
	"errors"
	"time"

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

const mediaCols = `id, file_name, file_type, file_url, file_size, uploaded_by, kind, created_at, updated_at`

func (s *PostgresStore) Create(ctx context.Context, m Media) (Media, error) {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	return scanMedia(s.pool.QueryRow(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+mediaCols, m.ID, m.Name, m.Type, m.Key, m.Size, m.UploadedBy, m.Kind))
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Media, error) {
	m, err := scanMedia(s.pool.QueryRow(ctx, `SELECT `+mediaCols+` FROM media WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Media{}, ErrNotFound
	}
	return m, err
}

func (s *PostgresStore) List(ctx context.Context) ([]Media, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Media, 0)
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID) (Media, error) {
	m, err := scanMedia(s.pool.QueryRow(ctx, `DELETE FROM media WHERE id = $1 RETURNING `+mediaCols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Media{}, ErrNotFound
	}
	return m, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMedia(row rowScanner) (Media, error) {
	var m Media
	var created, updated time.Time
	err := row.Scan(&m.ID, &m.Name, &m.Type, &m.Key, &m.Size, &m.UploadedBy, &m.Kind, &created, &updated)
	m.CreatedAt = created
	m.UpdatedAt = updated
	return m, err
}
