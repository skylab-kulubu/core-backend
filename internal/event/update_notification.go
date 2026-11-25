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

// Fields of a time or place change, as told_fields and the record of the
// editor's answer name them.
const (
	FieldStart    = "start"
	FieldEnd      = "end"
	FieldLocation = "location"
)

// changedTimeOrPlace reads which of the Event's start, end and location the
// update in tx changed from before; none when the Event was not news to
// its holders (a draft, cancelled or over).
func changedTimeOrPlace(ctx context.Context, tx pgx.Tx, id uuid.UUID, before *updateBefore) ([]string, error) {
	if before == nil || !before.told {
		return nil, nil
	}
	var start, end, place bool
	err := tx.QueryRow(ctx, `
		SELECT start_date IS DISTINCT FROM $2::timestamptz, end_date IS DISTINCT FROM $3::timestamptz,
			btrim(location) <> btrim($4::text)
		FROM events WHERE id = $1`, id, before.start, before.end, before.location).Scan(&start, &end, &place)
	if err != nil {
		return nil, err
	}
	var fields []string
	for _, f := range []struct {
		changed bool
		name    string
	}{{start, FieldStart}, {end, FieldEnd}, {place, FieldLocation}} {
		if f.changed {
			fields = append(fields, f.name)
		}
	}
	return fields, nil
}

// NotifyChanged turns the telling of an Event's other details on or off
// (EVENT_CHANGED_NOTIFICATION, docs/event-updated-notification.md#other-details):
// while on, a change of a published Event's name, description, attendee
// note, capacity or links joins the waiting notice when the editor says to
// tell; a body that does not say tells nothing of them.
func (s *PostgresStore) NotifyChanged(on bool) { s.notifyChanged.Store(on) }

// noticeChanges queues the update's change for the Event's Ticket holders
// and records the editor's answer (event_change_notice_choices): who saved
// which change, what they said, whether a notice was queued. Its time or
// place is told unless the editor said no, while the Notification is on;
// its other details only when the editor said yes, while their telling is
// on (NotifyChanged). A change that is no news (a draft's, an ended
// Event's, or one that left all as it was) records nothing; the other
// details are recorded only while their telling is on.
func (s *PostgresStore) noticeChanges(ctx context.Context, tx pgx.Tx, e Event, before *updateBefore) error {
	timeFields, err := changedTimeOrPlace(ctx, tx, e.ID, before)
	if err != nil {
		return err
	}
	var detailFields []string
	if s.notifyChanged.Load() && before != nil && before.told && e.detailsBefore != nil {
		detailFields = changedDetails(e.detailsBefore, DetailDigests(e))
	}
	fields := append(append([]string{}, timeFields...), detailFields...)
	if len(fields) == 0 {
		return nil
	}
	var told []string
	if len(timeFields) > 0 && s.notifyUpdated.Load() && (e.notifyHolders == nil || *e.notifyHolders) {
		told = append(told, timeFields...)
	}
	beforeDetails := map[string]string{}
	if len(detailFields) > 0 && e.notifyHolders != nil && *e.notifyHolders {
		told = append(told, detailFields...)
		for _, field := range detailFields {
			beforeDetails[field] = e.detailsBefore[field]
		}
	}
	queued := len(told) > 0
	if queued {
		if err := queueUpdateNotification(ctx, tx, e.ID, before, told, beforeDetails); err != nil {
			return err
		}
	}
	// decided_by names a users row or nobody: a principal without one (a
	// service account) is recorded as nobody rather than refusing the save.
	_, err = tx.Exec(ctx, `
		INSERT INTO event_change_notice_choices (id, event_id, decided_by, fields, notify, queued)
		VALUES ($1, $2, (SELECT u.id FROM users u WHERE u.id = $3::uuid), $4, $5, $6)`,
		uuid.New(), e.ID, e.changedBy, fields, e.notifyHolders, queued)
	return err
}

// queueUpdateNotification queues the Event's change for its Ticket holders.
// The row gathers the changes until the worker first takes it: while one
// waits, a later change adds no row (it keeps the first change's before),
// adds the fields it changed to the row's told_fields and the digests of
// details it is the first to tell, and the worker reads the Event as it is
// when it sends. The later change's write of the waiting row also keeps it
// locked until the change commits: a worker claiming it in between (FOR
// UPDATE SKIP LOCKED) skips it instead of reading the Event before the
// change and losing it.
func queueUpdateNotification(ctx context.Context, tx pgx.Tx, id uuid.UUID, before *updateBefore, fields []string, beforeDetails map[string]string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO event_update_notifications (id, event_id, before_start_date, before_end_date, before_location, told_fields, before_details, next_attempt_at)
		SELECT $1, e.id, $3::timestamptz, $4::timestamptz, $5::text, $6::text[], $9::jsonb, now() + make_interval(secs => $7)
		FROM events e
		WHERE e.id = $2 AND e.status = 'published'
		ON CONFLICT (event_id) WHERE started_at IS NULL DO UPDATE SET
			told_fields = ARRAY(
				SELECT DISTINCT f FROM unnest(COALESCE(event_update_notifications.told_fields, $8::text[]) || EXCLUDED.told_fields) f ORDER BY f),
			-- The first change's digests win: the right-hand side of || wins.
			before_details = EXCLUDED.before_details || event_update_notifications.before_details`,
		uuid.New(), id, before.start, before.end, before.location, fields, UpdateNotificationWindow.Seconds(),
		[]string{FieldStart, FieldEnd, FieldLocation}, beforeDetails)
	return err
}
