package mapregion

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/mapimage"
)

// Store keeps the regions (table map_regions, migration 20261122170000).
type Store struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const regionCols = `id, name, min_lon, min_lat, max_lon, max_lat, max_zoom, status, COALESCE(object_key, ''), COALESCE(source_build, ''),
	size_bytes, progress_bytes, COALESCE(error_code, ''), attempts, created_at, ready_at`

func scanRegion(row pgx.Row) (Region, error) {
	var r Region
	err := row.Scan(&r.ID, &r.Name, &r.Box.MinLon, &r.Box.MinLat, &r.Box.MaxLon, &r.Box.MaxLat, &r.MaxZoom, &r.Status, &r.ObjectKey,
		&r.SourceBuild, &r.SizeBytes, &r.ProgressBytes, &r.ErrorCode, &r.Attempts, &r.CreatedAt, &r.ReadyAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Region{}, ErrNotFound
	}
	return r.withBox(), err
}

func (s *Store) list(ctx context.Context, where string, args ...any) ([]Region, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+regionCols+` FROM map_regions `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Region, 0)
	for rows.Next() {
		r, err := scanRegion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// List reads every region, the newest first.
func (s *Store) List(ctx context.Context) ([]Region, error) {
	return s.list(ctx, `ORDER BY created_at DESC, id`)
}

// ListReady reads the ready regions, the smallest box first.
func (s *Store) ListReady(ctx context.Context) ([]Region, error) {
	return s.list(ctx, `WHERE status = 'ready' ORDER BY (max_lon - min_lon) * (max_lat - min_lat), id`)
}

// Get reads one region.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Region, error) {
	return scanRegion(s.pool.QueryRow(ctx, `SELECT `+regionCols+` FROM map_regions WHERE id = $1`, id))
}

// Create stores a requested region; ErrExists when a live one has its box.
func (s *Store) Create(ctx context.Context, name string, box mapimage.BBox, maxZoom int) (Region, error) {
	r, err := scanRegion(s.pool.QueryRow(ctx, `INSERT INTO map_regions (id, name, min_lon, min_lat, max_lon, max_lat, max_zoom)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+regionCols,
		uuid.New(), name, box.MinLon, box.MinLat, box.MaxLon, box.MaxLat, maxZoom))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Region{}, ErrExists
	}
	return r, err
}

// transition moves a region from one of the statuses to another;
// ErrNotFound, or ErrState when it is in another status.
func (s *Store) transition(ctx context.Context, id uuid.UUID, from []Status, set string) (Region, error) {
	names := make([]string, len(from))
	for i, f := range from {
		names[i] = string(f)
	}
	r, err := scanRegion(s.pool.QueryRow(ctx, `UPDATE map_regions SET `+set+`, updated_at = now()
		WHERE id = $1 AND status = ANY($2::text[]) RETURNING `+regionCols, id, names))
	if !errors.Is(err, ErrNotFound) {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Region{}, ErrExists
		}
		return r, err
	}
	if _, getErr := s.Get(ctx, id); getErr != nil {
		return Region{}, getErr
	}
	return Region{}, ErrState
}

// Retry asks again for a failed region.
func (s *Store) Retry(ctx context.Context, id uuid.UUID) (Region, error) {
	return s.transition(ctx, id, []Status{StatusFailed},
		`status = 'requested', error_code = NULL, progress_bytes = 0, claim_id = NULL, claimed_until = NULL`)
}

// Retire stops using a ready or failed region; its object stays.
func (s *Store) Retire(ctx context.Context, id uuid.UUID) (Region, error) {
	return s.transition(ctx, id, []Status{StatusReady, StatusFailed, StatusRequested},
		`status = 'retired', claim_id = NULL, claimed_until = NULL`)
}

// Claim is a worker's claim on one region.
type Claim struct {
	Region Region
	ID     uuid.UUID
}

// ClaimNext claims, in a short transaction, the oldest requested region, or
// one whose extraction's lease ran out (a worker that stopped): it becomes
// extracting, its try counted.
func (s *Store) ClaimNext(ctx context.Context, now time.Time, lease time.Duration) (Claim, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Claim{}, false, err
	}
	defer tx.Rollback(ctx)
	r, err := scanRegion(tx.QueryRow(ctx, `SELECT `+regionCols+` FROM map_regions
		WHERE status = 'requested' OR (status = 'extracting' AND claimed_until <= $1)
		ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, ErrNotFound) {
		return Claim{}, false, nil
	}
	if err != nil {
		return Claim{}, false, err
	}
	claim := Claim{Region: r, ID: uuid.New()}
	if _, err := tx.Exec(ctx, `UPDATE map_regions SET status = 'extracting', claim_id = $2, claimed_until = $3,
			attempts = attempts + 1, progress_bytes = 0, updated_at = now()
		WHERE id = $1`, r.ID, claim.ID, now.Add(lease)); err != nil {
		return Claim{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, false, err
	}
	claim.Region.Status = StatusExtracting
	return claim, true, nil
}

// Progress records the extract's size and the bytes uploaded, and extends
// the claim's lease. False when the claim is no longer this one.
func (s *Store) Progress(ctx context.Context, c Claim, size, progress int64, until time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE map_regions SET size_bytes = $3, progress_bytes = $4, claimed_until = $5, updated_at = now()
		WHERE id = $1 AND claim_id = $2`, c.Region.ID, c.ID, size, progress, until)
	return tag.RowsAffected() == 1, err
}

// Ready marks the extract done, and lets the Events in its box whose map
// was given up (no region held them, or the renderer refused) be drawn
// again.
func (s *Store) Ready(ctx context.Context, c Claim, key, build string, size int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE map_regions SET status = 'ready', object_key = $3, source_build = $4, size_bytes = $5,
			progress_bytes = $5, error_code = NULL, ready_at = now(), claim_id = NULL, claimed_until = NULL, updated_at = now()
		WHERE id = $1 AND claim_id = $2`, c.Region.ID, c.ID, key, build, size)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	b := c.Region.Box
	if _, err := tx.Exec(ctx, `UPDATE events SET map_state = NULL, map_attempts = 0, map_retry_at = NULL
		WHERE map_state = 'failed' AND map_media_id IS NULL AND archived_at IS NULL
		  AND location_lat BETWEEN $1 AND $2 AND location_lng BETWEEN $3 AND $4`, b.MinLat, b.MaxLat, b.MinLon, b.MaxLon); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Fail marks the extraction failed, with why.
func (s *Store) Fail(ctx context.Context, c Claim, code string) error {
	_, err := s.pool.Exec(ctx, `UPDATE map_regions SET status = 'failed', error_code = $3, claim_id = NULL, claimed_until = NULL, updated_at = now()
		WHERE id = $1 AND claim_id = $2`, c.Region.ID, c.ID, code)
	return err
}

// Release lets go of the claim without a verdict (core stopping): the
// region is requested again.
func (s *Store) Release(ctx context.Context, c Claim) error {
	_, err := s.pool.Exec(ctx, `UPDATE map_regions SET status = 'requested', progress_bytes = 0, claim_id = NULL, claimed_until = NULL, updated_at = now()
		WHERE id = $1 AND claim_id = $2`, c.Region.ID, c.ID)
	return err
}
