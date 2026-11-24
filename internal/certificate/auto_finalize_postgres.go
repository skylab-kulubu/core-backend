package certificate

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// autoFinalizeLock is the advisory lock the automatic finalization's pass
// takes: one replica plans at a time, as the feedback mail's planLock.
const autoFinalizeLock = 1499841308389431515

// autoFinalizeDueSQL are the Events due at $1: published and current, the
// rule issuing, the switch on, not closed, with a live Session, the last
// live Session's end ($2 s before $1 or earlier, and within the $3 s
// window). The Session's end is the feedback mail's sessionEndsSQL; every
// live Session counts, not only the counted ones.
const autoFinalizeDueSQL = `
	SELECT e.id
	FROM events e
	JOIN event_days d ON d.event_id = e.id AND d.archived_at IS NULL
	JOIN sessions s ON s.event_day_id = d.id AND s.archived_at IS NULL AND NOT s.cancelled
	WHERE e.archived_at IS NULL AND e.status = 'published'
	  AND e.attendance_rule <> 'none' AND e.attendance_auto_finalize
	  AND NOT EXISTS (SELECT 1 FROM certificate_event_state f WHERE f.event_id = e.id)
	GROUP BY e.id
	HAVING max(` + sessionEndsSQL + `) + $2::float8 * interval '1 second' <= $1
	   AND max(` + sessionEndsSQL + `) + $2::float8 * interval '1 second' > $1 - $3::float8 * interval '1 second'
	ORDER BY max(` + sessionEndsSQL + `), e.id`

// AutoFinalizeDue takes the pass's advisory lock in a transaction, reads the
// Events due at now and calls each with them while it holds the lock: a
// second replica's pass skips instead of closing the same Events. Each
// Event's finalization writes on its own (CreateBatch); the state row's
// key keeps it to one batch however the passes and the manual
// finalization meet.
func (s *PostgresStore) AutoFinalizeDue(ctx context.Context, now time.Time, each func(eventID uuid.UUID)) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(autoFinalizeLock)).Scan(&locked); err != nil || !locked {
		return false, err
	}
	rows, err := tx.Query(ctx, autoFinalizeDueSQL, now, AutoFinalizeDelay.Seconds(), AutoFinalizeWindow.Seconds())
	if err != nil {
		return true, err
	}
	due, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return true, err
	}
	for _, id := range due {
		if ctx.Err() != nil {
			break
		}
		each(id)
	}
	return true, tx.Commit(ctx)
}
