package ticket

import (
	"context"
	"errors"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

// NotifyStatus turns the ticket status mails on or off (TICKET_STATUS_MAIL,
// docs/event-capacity.md): while on, a Ticket put on the waiting list
// queues "Bekleme listesindesin" (event.waitlist-joined) and one promoted
// "Yer açıldı" (event.waitlist-promoted) in the transaction that writes it.
// Off, the waiting list queues nothing and a promoted Ticket gets the new
// Ticket's mail while that is on.
func (s *PostgresStore) NotifyStatus(on bool) { s.notifyStatus.Store(on) }

// promotionMail is what a Ticket this store promotes is told by.
func (s *PostgresStore) promotionMail() event.PromotionMail {
	return event.PromotionMailFor(s.notifyCreated.Load(), s.notifyStatus.Load())
}

// lockEvent locks the Event of the Ticket id names, before the Ticket: every
// write that changes a Ticket's status takes the Event's row first, then
// the Ticket's, so two of them never wait on each other in a circle.
func lockEvent(ctx context.Context, tx pgx.Tx, id uuid.UUID) (event.Capacity, error) {
	var eventID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT event_id FROM tickets WHERE id = $1`, id).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return event.Capacity{}, ErrNotFound
	}
	if err != nil {
		return event.Capacity{}, err
	}
	c, err := event.LockCapacity(ctx, tx, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return event.Capacity{}, ErrNotFound
	}
	return c, err
}

// lockStatus locks the Ticket's row and reads its status.
func lockStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Status, error) {
	var status Status
	err := tx.QueryRow(ctx, `SELECT status FROM tickets WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return status, err
}

// CreateAdmitted writes t under its Event's lock: active while the capacity
// takes it (or a.Over), else waitlisted (a.Waitlist and the waiting list
// on), else ErrEventFull. The Ticket's mail is queued in the same
// transaction: "Biletin hazır" for an active one while the Notifications
// are on, "Bekleme listesindesin" for a waitlisted one while the status
// mails are; none with a.Quiet.
func (s *PostgresStore) CreateAdmitted(ctx context.Context, t Ticket, a Admission) (Ticket, error) {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	var got Ticket
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := event.LockCapacity(ctx, tx, t.EventID)
		if errors.Is(err, event.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		status, over, err := admit(c, a)
		if err != nil {
			return err
		}
		if got, err = s.insertTicket(ctx, tx, t, status); err != nil {
			return err
		}
		if a.FormResponse != uuid.Nil {
			if _, err := tx.Exec(ctx, `UPDATE tickets SET form_response_id = $2 WHERE id = $1`, got.ID, a.FormResponse); err != nil {
				return err
			}
		}
		got.OverCapacity = over
		switch {
		case a.Quiet:
			// An import whose mail is off: no mail now.
		case status == StatusActive && s.notifyCreated.Load():
			_, err = tx.Exec(ctx, `INSERT INTO ticket_notifications (ticket_id) VALUES ($1)`, got.ID)
		case status == StatusWaitlisted:
			err = s.queueWaitlisted(ctx, tx, got)
		}
		return err
	})
	if err != nil {
		return Ticket{}, err
	}
	got.CheckIns = []CheckIn{}
	return got, nil
}

// queueWaitlisted queues "Bekleme listesindesin" for t while the status
// mails are on; off, the log says the mail did not go, naming the Event
// alone.
func (s *PostgresStore) queueWaitlisted(ctx context.Context, tx pgx.Tx, t Ticket) error {
	if !s.notifyStatus.Load() {
		log.Printf("event %s: a registration went on the waitlist; no mail (TICKET_STATUS_MAIL is not on)", t.EventID)
		return nil
	}
	return event.QueueTicketMail(ctx, tx, t.ID, "waitlisted")
}

// insertTicket writes t of status in tx with a fresh access key and code,
// each draw in a savepoint of its own: a code or key another Ticket holds is
// drawn again.
func (s *PostgresStore) insertTicket(ctx context.Context, tx pgx.Tx, t Ticket, status Status) (Ticket, error) {
	for attempt := 1; ; attempt++ {
		accessKey, err := s.newAccessKey()
		if err != nil {
			return Ticket{}, err
		}
		code, err := s.newCode()
		if err != nil {
			return Ticket{}, err
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return Ticket{}, err
		}
		got, err := scanTicket(sp.QueryRow(ctx, `
			INSERT INTO tickets (
				id, event_id, ticket_type, owner_id,
				guest_first_name, guest_last_name, guest_email, guest_phone_number,
				access_key, code, status, status_changed_at, waitlisted_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,
				CASE WHEN $11 = 'waitlisted' THEN now() END, CASE WHEN $11 = 'waitlisted' THEN now() END)
			RETURNING `+ticketCols,
			t.ID, t.EventID, t.TicketType, t.OwnerID,
			t.GuestFirstName, t.GuestLastName, t.GuestEmail, t.GuestPhoneNumber,
			accessKey, storedCode(code), string(status)))
		if err != nil {
			_ = sp.Rollback(ctx)
			switch {
			case subjectlock.IsInactiveAccountReference(err):
				return Ticket{}, ErrNotFound
			case drawnTaken(err) && attempt < createAttempts:
				continue
			case isUnique(err) && !drawnTaken(err):
				// The Event already has a Ticket for the owner or the guest
				// e-mail, written since the caller looked.
				return Ticket{}, ErrConflict
			}
			return Ticket{}, err
		}
		return got, sp.Commit(ctx)
	}
}

// ReopenAdmitted reopens a cancelled Ticket under its Event's lock, active
// or waitlisted as a lets (CreateAdmitted), with a new access key and the
// next QR version. An active one's mail goes again under the next resend
// number while the Notifications are on; a waitlisted one is told it waits
// while the status mails are on. The forms answer the Ticket came from is
// a.FormResponse's now (none when another way reopens it).
func (s *PostgresStore) ReopenAdmitted(ctx context.Context, id uuid.UUID, a Admission) (Ticket, bool, error) {
	var (
		got     Ticket
		changed bool
		over    bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := lockEvent(ctx, tx, id)
		if err != nil {
			return err
		}
		current, err := lockStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		if current == StatusCancelled {
			status, beyond, err := admit(c, a)
			if err != nil {
				return err
			}
			key, err := s.newAccessKey()
			if err != nil {
				return err
			}
			// An erased holder's Ticket keeps no key (erasure nulls it).
			if _, err := tx.Exec(ctx, `
				UPDATE tickets SET status = $3, status_changed_at = now(),
					waitlisted_at = CASE WHEN $3 = 'waitlisted' THEN now() END,
					cancelled_at = NULL, cancelled_by = NULL, cancel_reason = NULL,
					access_key = CASE WHEN access_key IS NULL THEN NULL ELSE $2 END,
					qr_version = qr_version + 1, updated_at = now(),
					form_response_id = $4
				WHERE id = $1`, id, key, string(status), formResponse(a)); err != nil {
				return err
			}
			changed, over = true, beyond
			got, err = scanTicket(tx.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE id = $1`, id))
			if err != nil {
				return err
			}
			switch {
			case status == StatusActive && s.notifyCreated.Load():
				return queueReopened(ctx, tx, id)
			case status == StatusWaitlisted:
				return s.queueWaitlisted(ctx, tx, got)
			}
			return nil
		}
		got, err = scanTicket(tx.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return Ticket{}, false, err
	}
	got.CheckIns = []CheckIn{}
	got.OverCapacity = over
	return got, changed, nil
}

// formResponse is the forms answer a reopening keeps on the Ticket: the
// one reopening it, none for any other way (docs/form-response-tickets.md).
func formResponse(a Admission) *uuid.UUID {
	if a.FormResponse == uuid.Nil {
		return nil
	}
	return &a.FormResponse
}

// Promote makes a waitlisted Ticket active under its Event's lock, past the
// capacity if it must, and queues its mail (event.PromoteWaitlist's). One
// that is not waitlisted is answered as it is, changed false.
func (s *PostgresStore) Promote(ctx context.Context, id uuid.UUID) (Ticket, bool, error) {
	var (
		got     Ticket
		changed bool
		over    bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := lockEvent(ctx, tx, id)
		if err != nil {
			return err
		}
		status, err := lockStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		if status == StatusWaitlisted {
			if _, err := tx.Exec(ctx, `
				UPDATE tickets SET status = 'active', status_changed_at = now(), waitlisted_at = NULL, updated_at = now()
				WHERE id = $1`, id); err != nil {
				return err
			}
			if err := event.QueuePromotedMail(ctx, tx, id, s.promotionMail()); err != nil {
				return err
			}
			changed, over = true, overCapacity(c)
		}
		got, err = scanTicket(tx.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return Ticket{}, false, err
	}
	got.CheckIns = []CheckIn{}
	got.OverCapacity = over
	return got, changed, nil
}

// Capacity is the Event's capacity now, unlocked.
func (s *PostgresStore) Capacity(ctx context.Context, eventID uuid.UUID) (event.Capacity, error) {
	c, err := event.ReadCapacity(ctx, s.pool, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return event.Capacity{}, ErrNotFound
	}
	return c, err
}

var _ CapacityStore = (*PostgresStore)(nil)
