package formsfeed

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kinds of pull (form_pulls.kind).
const (
	// PullBackfill is an organizer's "Bu kişilere de bilet yaz": every
	// accepted answer of the form.
	PullBackfill = "backfill"
	// PullCatchup is the daily pass: the answers changed since the form's
	// watermark − WatermarkMargin.
	PullCatchup = "catchup"
)

const (
	// WatermarkMargin is how far before its watermark a form's daily pass
	// asks: an answer committed in Forms around the watermark's time, or a
	// clock a little apart, is not missed (SPEC §10 eki). A repeat is
	// harmless.
	WatermarkMargin = 5 * time.Minute
	// CatchupEvery is how often a form's daily pass runs.
	CatchupEvery = 24 * time.Hour
	// pullLease is how long a claimed job is kept from another worker: a
	// replica that stopped mid-job leaves it to be taken again then.
	pullLease = 15 * time.Minute
	// planLock is the advisory lock of the daily pass's planning: one
	// replica plans at a time.
	planLock = 7206119531226841977
	// watermarkRetention is how long the watermark of a form no current
	// Event lists is kept.
	watermarkRetention = 30 * 24 * time.Hour
)

// PullJob is one row of form_pulls.
type PullJob struct {
	ID           uuid.UUID
	FormID       uuid.UUID
	Kind         string
	EventID      *uuid.UUID
	RequestedAt  time.Time
	ChangedSince *time.Time
	Cursor       string
	Attempts     int
}

// PullQueue is the jobs and the watermarks (PostgresPulls).
type PullQueue interface {
	// Backfill queues an organizer's pull of the form; false when one is
	// waiting already.
	Backfill(ctx context.Context, formID, eventID, requestedBy uuid.UUID, now time.Time) (bool, error)
	// Claim takes the next due job and keeps it from others for pullLease.
	Claim(ctx context.Context, now time.Time) (PullJob, bool, error)
	// SaveCursor keeps the next page's cursor of a running job.
	SaveCursor(ctx context.Context, id uuid.UUID, cursor string) error
	// Finish deletes a done job and moves its form's watermark forward to
	// watermark (nil leaves it).
	Finish(ctx context.Context, job PullJob, watermark *time.Time, now time.Time) error
	// Retry counts a failed try and sets the next.
	Retry(ctx context.Context, id uuid.UUID, attempts int, next time.Time) error
	// Drop deletes a job given up.
	Drop(ctx context.Context, id uuid.UUID) error
	// Plan is the daily pass's planning over the forms current Events
	// list: a form seen the first time gets its watermark (now) and its
	// first pass a day later; a form whose pass is due gets a catch-up job.
	// One replica plans at a time; another answers 0 at once.
	Plan(ctx context.Context, forms []uuid.UUID, now time.Time) (int, error)
}

// PostgresPulls is PullQueue on form_pulls and form_pull_watermarks.
type PostgresPulls struct{ pool *pgxpool.Pool }

// NewPostgresPulls builds the queue on pool.
func NewPostgresPulls(pool *pgxpool.Pool) *PostgresPulls { return &PostgresPulls{pool: pool} }

func (s *PostgresPulls) Backfill(ctx context.Context, formID, eventID, requestedBy uuid.UUID, now time.Time) (bool, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO form_pulls (id, form_id, kind, event_id, requested_by, requested_at, next_attempt_at)
		VALUES ($1, $2, 'backfill', $3, $4, $5, $5)
		ON CONFLICT (form_id, kind) DO NOTHING
		RETURNING id`, uuid.New(), formID, eventID, requestedBy, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *PostgresPulls) Claim(ctx context.Context, now time.Time) (PullJob, bool, error) {
	var j PullJob
	var cursor *string
	err := s.pool.QueryRow(ctx, `
		UPDATE form_pulls SET next_attempt_at = $2
		WHERE id = (
			SELECT id FROM form_pulls WHERE next_attempt_at <= $1
			ORDER BY next_attempt_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1)
		RETURNING id, form_id, kind, event_id, requested_at, changed_since, cursor, attempts`,
		now, now.Add(pullLease)).Scan(&j.ID, &j.FormID, &j.Kind, &j.EventID, &j.RequestedAt, &j.ChangedSince, &cursor, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return PullJob{}, false, nil
	}
	if err != nil {
		return PullJob{}, false, err
	}
	if cursor != nil {
		j.Cursor = *cursor
	}
	return j, true, nil
}

func (s *PostgresPulls) SaveCursor(ctx context.Context, id uuid.UUID, cursor string) error {
	_, err := s.pool.Exec(ctx, `UPDATE form_pulls SET cursor = NULLIF($2, '') WHERE id = $1`, id, cursor)
	return err
}

func (s *PostgresPulls) Finish(ctx context.Context, job PullJob, watermark *time.Time, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM form_pulls WHERE id = $1`, job.ID); err != nil {
		return err
	}
	if watermark != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO form_pull_watermarks AS w (form_id, watermark, next_catchup_at, updated_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (form_id) DO UPDATE SET watermark = GREATEST(w.watermark, EXCLUDED.watermark), updated_at = EXCLUDED.updated_at`,
			job.FormID, watermark.UTC(), now.Add(CatchupEvery), now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresPulls) Retry(ctx context.Context, id uuid.UUID, attempts int, next time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE form_pulls SET attempts = $2, next_attempt_at = $3 WHERE id = $1`, id, attempts, next)
	return err
}

func (s *PostgresPulls) Drop(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM form_pulls WHERE id = $1`, id)
	return err
}

func (s *PostgresPulls) Plan(ctx context.Context, forms []uuid.UUID, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(planLock)).Scan(&locked); err != nil || !locked {
		return 0, err
	}
	if forms == nil {
		forms = []uuid.UUID{}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO form_pull_watermarks (form_id, watermark, next_catchup_at, updated_at)
		SELECT f, $2, $3, $2 FROM unnest($1::uuid[]) AS f
		ON CONFLICT (form_id) DO NOTHING`, forms, now, now.Add(CatchupEvery)); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
		WITH due AS (
			UPDATE form_pull_watermarks SET next_catchup_at = $3
			WHERE form_id = ANY($1::uuid[]) AND next_catchup_at <= $2
			RETURNING form_id, watermark)
		INSERT INTO form_pulls (id, form_id, kind, changed_since, requested_at, next_attempt_at)
		SELECT gen_random_uuid(), form_id, 'catchup', watermark - $4::float8 * interval '1 second', $2, $2 FROM due
		ON CONFLICT (form_id, kind) DO NOTHING`, forms, now, now.Add(CatchupEvery), WatermarkMargin.Seconds())
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM form_pull_watermarks
		WHERE NOT (form_id = ANY($1::uuid[])) AND updated_at < $2`, forms, now.Add(-watermarkRetention)); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), tx.Commit(ctx)
}
