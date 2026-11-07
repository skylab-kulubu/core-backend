package event

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UpdateNotificationWindow is how long the changes of an Event's time or
// place are gathered before its Ticket holders are told
// (docs/event-updated-notification.md): an organizer fixing the start, then
// the end, then the place sends one Notification, and a change undone within
// it sends none.
const UpdateNotificationWindow = 15 * time.Minute

// NotifyUpdated turns the Event change Notification on or off: while on,
// Update also queues a change of a published Event's start, end or
// location in event_update_notifications, in the same transaction, for the
// worker (internal/eventupdatenotify) to send.
func (s *PostgresStore) NotifyUpdated(on bool) { s.notifyUpdated.Store(on) }

// updateBefore is what the Event said before an Update, read under the
// row's lock.
type updateBefore struct {
	start, end *time.Time
	location   string
	// told: the Event was published and not over, so a change of it is
	// news to its Ticket holders. A draft's, a cancelled and an ended
	// Event's changes tell nobody.
	told bool
}

// readUpdateBefore locks the Event's row and reads it; nil when there is no
// such current Event (the Update then finds none either).
func readUpdateBefore(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*updateBefore, error) {
	var b updateBefore
	err := tx.QueryRow(ctx, `
		SELECT e.start_date, e.end_date, e.location,
			e.status = 'published' AND (`+endsAtSQL+` IS NULL OR `+endsAtSQL+` > now())
		FROM events e WHERE e.id = $1 AND e.archived_at IS NULL FOR UPDATE`, id).Scan(&b.start, &b.end, &b.location, &b.told)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// queueUpdateNotification queues the Event's change for its Ticket holders
// when its start, end or location is not what before read. The row gathers
// the changes until the worker first takes it: while one waits, a later
// change adds no row (it keeps the first change's before), and the worker
// reads the Event as it is when it sends. A later change still writes the
// waiting row (DO UPDATE, a no-op), so the row stays locked until the change
// commits: a worker claiming it in between (FOR UPDATE SKIP LOCKED) skips it
// instead of reading the Event before the change and losing it.
func queueUpdateNotification(ctx context.Context, tx pgx.Tx, id uuid.UUID, before *updateBefore) error {
	if before == nil || !before.told {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO event_update_notifications (id, event_id, before_start_date, before_end_date, before_location, next_attempt_at)
		SELECT $1, e.id, $3::timestamptz, $4::timestamptz, $5::text, now() + make_interval(secs => $6)
		FROM events e
		WHERE e.id = $2 AND e.status = 'published'
		  AND (e.start_date IS DISTINCT FROM $3::timestamptz OR e.end_date IS DISTINCT FROM $4::timestamptz
			OR btrim(e.location) <> btrim($5::text))
		ON CONFLICT (event_id) WHERE started_at IS NULL DO UPDATE SET event_id = EXCLUDED.event_id`,
		uuid.New(), id, before.start, before.end, before.location, UpdateNotificationWindow.Seconds())
	return err
}
