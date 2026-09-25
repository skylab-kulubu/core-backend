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
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
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

func (s *PostgresStore) GetByRetiredAlias(ctx context.Context, alias string) (URL, error) {
	u, err := scanURL(s.pool.QueryRow(ctx, `
		SELECT `+urlCols+` FROM urls
		WHERE id = (SELECT url_id FROM url_retired_aliases WHERE alias = $1)
		  AND disabled_at IS NULL`, alias))
	return u, mapURLErr(err)
}

func (s *PostgresStore) ListByCreator(ctx context.Context, userID uuid.UUID) ([]URL, error) {
	return s.ListByCreatorLifecycle(ctx, userID, lifecycle.CurrentOnly)
}

func (s *PostgresStore) ListByCreatorLifecycle(ctx context.Context, userID uuid.UUID, visibility lifecycle.Visibility) ([]URL, error) {
	q := `SELECT ` + urlCols + ` FROM urls WHERE created_by = $1`
	if condition := visibility.SQLCondition("disabled_at"); condition != "" {
		q += ` AND ` + condition
	}
	return s.list(ctx, q+` ORDER BY created_at DESC`, userID)
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return URL{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var previous string
	if err := tx.QueryRow(ctx, `SELECT alias FROM urls WHERE id = $1 AND disabled_at IS NULL FOR UPDATE`, u.ID).Scan(&previous); err != nil {
		return URL{}, mapURLErr(err)
	}
	if previous != u.Alias {
		if _, err := tx.Exec(ctx, `
			INSERT INTO url_retired_aliases (alias, url_id) VALUES ($1, $2)
			ON CONFLICT (alias) DO UPDATE SET url_id = EXCLUDED.url_id, retired_at = now()`, previous, u.ID); err != nil {
			return URL{}, mapURLErr(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM url_retired_aliases WHERE lower(alias) = lower($1) AND url_id = $2`, u.Alias, u.ID); err != nil {
			return URL{}, mapURLErr(err)
		}
	}
	got, err := scanURL(tx.QueryRow(ctx, `
		UPDATE urls SET alias = $2, url = $3, updated_at = now()
		WHERE id = $1 AND disabled_at IS NULL
		RETURNING `+urlCols, u.ID, u.Alias, u.URL))
	if err != nil {
		return URL{}, mapURLErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return URL{}, err
	}
	return got, nil
}

func (s *PostgresStore) AliasTaken(ctx context.Context, alias string, except uuid.UUID) (bool, error) {
	var taken bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM urls WHERE lower(alias) = lower($1) AND id <> $2)
		    OR EXISTS (SELECT 1 FROM url_retired_aliases WHERE lower(alias) = lower($1) AND url_id IS DISTINCT FROM $2)`,
		alias, except).Scan(&taken)
	return taken, err
}

func (s *PostgresStore) Disable(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if actorID != nil {
		// RecordHit acquires the subject advisory lock before it mutates the URL
		// row. Do the same here before the disabled_by trigger can take the lock
		// after locking that row; inverse URL->subject ordering deadlocks with a
		// concurrent attributed hit.
		if err := subjectlock.Lock(ctx, tx, *actorID); err != nil {
			return err
		}
		var allowed bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM users WHERE id=$1 AND account_state='active')
			   AND NOT EXISTS (SELECT 1 FROM account_deletion_requests WHERE subject_id=$1)
		`, *actorID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrForbidden
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE urls
		SET disabled_at = now(), disabled_by = $2, updated_at = now()
		WHERE id = $1 AND disabled_at IS NULL`, id, actorID)
	if err != nil {
		if subjectlock.IsInactiveAccountReference(err) {
			return ErrForbidden
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM urls WHERE id=$1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) Restore(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE urls
		SET disabled_at = NULL,
			disabled_by = NULL,
			updated_at = CASE WHEN disabled_at IS NOT NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id)
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
	attributedUserID := hit.UserID
	if attributedUserID != nil {
		if err := subjectlock.Lock(ctx, tx, *attributedUserID); err != nil {
			return URL{}, err
		}
		var allowed bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND account_state = 'active')
			   AND NOT EXISTS (SELECT 1 FROM account_deletion_requests WHERE subject_id = $1)
		`, *attributedUserID).Scan(&allowed); err != nil {
			return URL{}, err
		}
		if !allowed {
			attributedUserID = nil
		}
	}
	var alias string
	if err := tx.QueryRow(ctx, `SELECT alias FROM urls WHERE id = $1 AND disabled_at IS NULL`, id).Scan(&alias); err != nil {
		return URL{}, mapURLErr(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO url_hits (
			id, url_id, alias, at, ip, user_agent, referer, user_id,
			utm_source, utm_medium, utm_campaign, utm_term, utm_content
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		hit.ID, id, alias, hit.CreatedAt, hit.IP, hit.UserAgent, hit.Referer, attributedUserID,
		hit.UTM.Source, hit.UTM.Medium, hit.UTM.Campaign, hit.UTM.Term, hit.UTM.Content); err != nil {
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
		SELECT id, url_id, alias, at, ip, user_agent, referer, user_id,
			utm_source, utm_medium, utm_campaign, utm_term, utm_content
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
		if err := rows.Scan(
			&h.ID, &h.URLID, &h.Alias, &h.CreatedAt, &h.IP, &h.UserAgent, &h.Referer, &h.UserID,
			&h.UTM.Source, &h.UTM.Medium, &h.UTM.Campaign, &h.UTM.Term, &h.UTM.Content,
		); err != nil {
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
	if subjectlock.IsInactiveAccountReference(err) {
		return ErrForbidden
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
