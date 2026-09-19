package media

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

const mediaCols = `id, file_name, file_type, file_url, file_size, uploaded_by, kind, cover_colors, cover_colors_computed, deleted_at, deleted_by, created_at, updated_at`

func (s *PostgresStore) Create(ctx context.Context, m Media) (Media, error) {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	if m.CoverColors == nil {
		m.CoverColors = []string{}
	}
	return scanMedia(s.pool.QueryRow(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, cover_colors, cover_colors_computed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+mediaCols, m.ID, m.Name, m.Type, m.Key, m.Size, m.UploadedBy, m.Kind, m.CoverColors, m.CoverColorsComputed))
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Media, error) {
	return s.get(ctx, id, false)
}

func (s *PostgresStore) GetIncludingDeleted(ctx context.Context, id uuid.UUID) (Media, error) {
	return s.get(ctx, id, true)
}

func (s *PostgresStore) get(ctx context.Context, id uuid.UUID, includeDeleted bool) (Media, error) {
	q := `SELECT ` + mediaCols + ` FROM media WHERE id = $1`
	if !includeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	m, err := scanMedia(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Media{}, ErrNotFound
	}
	return m, err
}

func (s *PostgresStore) List(ctx context.Context) ([]Media, error) {
	return s.list(ctx, lifecycle.CurrentOnly)
}

func (s *PostgresStore) ListLifecycle(ctx context.Context, visibility lifecycle.Visibility) ([]Media, error) {
	return s.list(ctx, visibility)
}

func (s *PostgresStore) list(ctx context.Context, visibility lifecycle.Visibility) ([]Media, error) {
	q := `SELECT ` + mediaCols + ` FROM media`
	if condition := visibility.SQLCondition("deleted_at"); condition != "" {
		q += ` WHERE ` + condition
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY created_at DESC`)
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

func (s *PostgresStore) ListPendingCoverColors(ctx context.Context, limit int) ([]Media, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media WHERE kind = $1 AND cover_colors_computed = false AND deleted_at IS NULL ORDER BY created_at LIMIT $2`, KindImage, limit)
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

func (s *PostgresStore) SetCoverColors(ctx context.Context, id uuid.UUID, colors []string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE media SET cover_colors = $2, cover_colors_computed = true, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, id, colors)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
	err := row.Scan(&m.ID, &m.Name, &m.Type, &m.Key, &m.Size, &m.UploadedBy, &m.Kind, &m.CoverColors, &m.CoverColorsComputed, &m.DeletedAt, &m.DeletedBy, &created, &updated)
	if m.CoverColors == nil {
		m.CoverColors = []string{}
	}
	m.CreatedAt = created
	m.UpdatedAt = updated
	return m, err
}
