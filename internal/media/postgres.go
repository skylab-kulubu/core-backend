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
