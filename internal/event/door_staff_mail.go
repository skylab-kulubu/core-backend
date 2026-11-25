package event

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DoorStaffMailDelay is how long a person added to the door staff waits for
// their mail (docs/door-staff-mail.md): added and taken off again within it,
// they get none.
const DoorStaffMailDelay = 2 * time.Minute

// NotifyDoorStaff turns the door staff mail on or off: while on, a write
// that adds a person to an Event's door staff also queues their mail in
// door_staff_notifications, in the same transaction, for the worker
// (internal/doorstaffmail) to send. Off, nothing is queued, so turning it
// on later tells none of the people added meanwhile.
func (s *PostgresStore) NotifyDoorStaff(on bool) { s.notifyDoorStaff.Store(on) }

// queueDoorStaffMail queues the mail of each person just added to the door
// staff who has a users row; a row still waiting for the same Event and
// person stays as it is.
func (s *PostgresStore) queueDoorStaffMail(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, added []uuid.UUID) error {
	if !s.notifyDoorStaff.Load() || len(added) == 0 {
		return nil
	}
	for _, id := range added {
		if _, err := tx.Exec(ctx, `
			INSERT INTO door_staff_notifications (id, event_id, user_id, next_attempt_at)
			SELECT $1, $2, u.id, now() + make_interval(secs => $4) FROM users u WHERE u.id = $3
			ON CONFLICT (event_id, user_id) DO NOTHING`,
			uuid.New(), eventID, id, DoorStaffMailDelay.Seconds()); err != nil {
			return err
		}
	}
	return nil
}
