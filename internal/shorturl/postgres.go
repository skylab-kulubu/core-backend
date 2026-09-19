package shorturl

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const urlCols = `id, alias, url, click_count, created_by, disabled_at, disabled_by, created_at, updated_at`

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
	return s.get(ctx, id, false)
}

func (s *PostgresStore) GetIncludingDisabled(ctx context.Context, id uuid.UUID) (URL, error) {
	return s.get(ctx, id, true)
}

func (s *PostgresStore) get(ctx context.Context, id uuid.UUID, includeDisabled bool) (URL, error) {
	q := `SELECT ` + urlCols + ` FROM urls WHERE id = $1`
	if !includeDisabled {
		q += ` AND disabled_at IS NULL`
	}
	u, err := scanURL(s.pool.QueryRow(ctx, q, id))
	return u, mapURLErr(err)
}

func (s *PostgresStore) GetByAlias(ctx context.Context, alias string) (URL, error) {
	u, err := scanURL(s.pool.QueryRow(ctx, `SELECT `+urlCols+` FROM urls WHERE alias = $1 AND disabled_at IS NULL`, alias))
	return u, mapURLErr(err)
}

func (s *PostgresStore) ListByCreator(ctx context.Context, userID uuid.UUID) ([]URL, error) {
	return s.list(ctx, `SELECT `+urlCols+` FROM urls WHERE created_by = $1 AND disabled_at IS NULL ORDER BY created_at DESC`, userID)
}

func (s *PostgresStore) ListAll(ctx context.Context) ([]URL, error) {
	return s.list(ctx, `SELECT `+urlCols+` FROM urls WHERE disabled_at IS NULL ORDER BY created_at DESC`)
}

func (s *PostgresStore) ListLifecycle(ctx context.Context, visibility lifecycle.Visibility) ([]URL, error) {
	q := `SELECT ` + urlCols + ` FROM urls`
	if condition := visibility.SQLCondition("disabled_at"); condition != "" {
		q += ` WHERE ` + condition
	}
	return s.list(ctx, q+` ORDER BY created_at DESC`)
}

func (s *PostgresStore) Update(ctx context.Context, u URL) (URL, error) {
	got, err := scanURL(s.pool.QueryRow(ctx, `
		UPDATE urls SET alias = $2, url = $3, updated_at = now()
		WHERE id = $1 AND disabled_at IS NULL
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

func (s *PostgresStore) RecordHit(ctx context.Context, id uuid.UUID, hit Hit) (URL, error) {
	if hit.ID == uuid.Nil {
		hit.ID = uuid.New()
	}
	if hit.CreatedAt.IsZero() {
		hit.CreatedAt = time.Now().UTC()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return URL{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var alias string
	if err := tx.QueryRow(ctx, `SELECT alias FROM urls WHERE id = $1 AND disabled_at IS NULL`, id).Scan(&alias); err != nil {
		return URL{}, mapURLErr(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO url_hits (id, url_id, alias, at, ip, user_agent, referer, user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		hit.ID, id, alias, hit.CreatedAt, hit.IP, hit.UserAgent, hit.Referer, hit.UserID); err != nil {
		return URL{}, mapURLErr(err)
	}
	u, err := scanURL(tx.QueryRow(ctx, `
		UPDATE urls SET click_count = click_count + 1, updated_at = now()
		WHERE id = $1 AND disabled_at IS NULL
		RETURNING `+urlCols, id))
	if err != nil {
		return URL{}, mapURLErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return URL{}, err
	}
	return u, nil
}

func (s *PostgresStore) PruneHits(ctx context.Context, before time.Time) error {
	_, err := s.pool.Exec(ctx, `
		WITH deleted AS (
			DELETE FROM url_hits
			WHERE at < $1
			RETURNING url_id
		), affected AS (
			SELECT url_id, COUNT(*) AS deleted_count
			FROM deleted
			GROUP BY url_id
		)
		UPDATE urls
		SET click_count = GREATEST(urls.click_count - affected.deleted_count, 0), updated_at = now()
		FROM affected
		WHERE urls.id = affected.url_id`, before)
	return err
}

func (s *PostgresStore) ListHits(ctx context.Context, id uuid.UUID, since time.Time) ([]Hit, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, url_id, alias, at, ip, user_agent, referer, user_id
		FROM url_hits
		WHERE url_id = $1 AND at >= $2
		ORDER BY at DESC, id DESC`, id, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Hit, 0)
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ID, &h.URLID, &h.Alias, &h.CreatedAt, &h.IP, &h.UserAgent, &h.Referer, &h.UserID); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
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
	err := row.Scan(&u.ID, &u.Alias, &u.URL, &u.ClickCount, &u.CreatedBy, &u.DisabledAt, &u.DisabledBy, &created, &updated)
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
