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
// live Session's end (an Event with no live Session: its own end, once a
// Ticket has a Venue entry) ($2 s before $1 or earlier, and within the $3 s
// window). The Session's end is the feedback mail's sessionEndsSQL; every
// live Session counts, not only the counted ones.
const autoFinalizeDueSQL = `
	WITH ends AS (
		SELECT e.id, max(` + sessionEndsSQL + `) AS ends_at
		FROM events e
		JOIN event_days d ON d.event_id = e.id AND d.archived_at IS NULL
		JOIN sessions s ON s.event_day_id = d.id AND s.archived_at IS NULL AND NOT s.cancelled
		WHERE e.archived_at IS NULL AND e.status = 'published'
		  AND e.attendance_rule <> 'none' AND e.attendance_auto_finalize
		  AND NOT EXISTS (SELECT 1 FROM certificate_event_state f WHERE f.event_id = e.id)
		GROUP BY e.id
		UNION ALL
		-- An Event with no live Session closes at its own end, once someone
		-- entered it (docs/certificate-rule.md, "An Event with no Session").
		SELECT e.id, ` + eventEndsSQL + `
		FROM events e
		WHERE e.archived_at IS NULL AND e.status = 'published'
		  AND e.attendance_rule <> 'none' AND e.attendance_auto_finalize
		  AND NOT EXISTS (SELECT 1 FROM certificate_event_state f WHERE f.event_id = e.id)
		  AND NOT EXISTS (
			SELECT 1 FROM event_days d
			JOIN sessions s ON s.event_day_id = d.id AND s.archived_at IS NULL AND NOT s.cancelled
			WHERE d.event_id = e.id AND d.archived_at IS NULL)
		  AND EXISTS (
			SELECT 1 FROM venue_entries v
			JOIN tickets t ON t.id = v.ticket_id AND t.status = 'active'
			WHERE v.event_id = e.id)
	)
	SELECT id FROM ends
	WHERE ends_at + $2::float8 * interval '1 second' <= $1
	  AND ends_at + $2::float8 * interval '1 second' > $1 - $3::float8 * interval '1 second'
	ORDER BY ends_at, id`

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
