package event

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// An Event's capacity and waiting list (kontenjan-iptal-hatirlatma 06, 07;
// docs/event-capacity.md). Every write that may make a Ticket active, and
// every change that may free a place, holds the Event's row (FOR UPDATE)
// while it counts and writes: two registrations for the last place, or a
// cancellation and a registration, never both get it.

// Waitlist reports whether e puts a registration on its waiting list once
// full: on unless WaitlistEnabled says off.
func (e Event) Waitlist() bool {
	return e.WaitlistEnabled == nil || *e.WaitlistEnabled
}

// StartedAt reports whether e has started at now: an undated Event never
// starts. Promotion from the waiting list stops then.
func (e Event) StartedAt(now time.Time) bool {
	return e.StartDate != nil && !e.StartDate.After(now)
}

// Capacity is an Event's capacity as a Ticket's write reads it, under the
// Event's lock (LockCapacity).
type Capacity struct {
	// Capacity is the Event's bound, 0 for none; Active its active Tickets.
	Capacity int
	Active   int
	// Waitlist is the Event's waiting list switch.
	Waitlist bool
	// Promotable: the waiting list moves now (published, not archived, not
	// started).
	Promotable bool
}

// Full reports whether the active Tickets fill the capacity.
func (c Capacity) Full() bool {
	return c.Capacity > 0 && c.Active >= c.Capacity
}

// Free is how many more Tickets the capacity takes: -1 for no bound.
func (c Capacity) Free() int {
	if c.Capacity <= 0 {
		return -1
	}
	return max(c.Capacity-c.Active, 0)
}

// CapacityOf is e's Capacity with active active Tickets, at now.
func CapacityOf(e Event, active int, now time.Time) Capacity {
	return Capacity{
		Capacity:   e.Capacity,
		Active:     active,
		Waitlist:   e.Waitlist(),
		Promotable: e.Status == StatusPublished && e.ArchivedAt == nil && !e.StartedAt(now),
	}
}

// capacitySQL reads the Event's capacity, its waiting list switch, whether
// it is promotable and its active Tickets; locked, it holds the Event's row
// until the transaction ends.
const capacitySQL = `
	SELECT e.capacity, e.waitlist_enabled,
		e.status = 'published' AND e.archived_at IS NULL AND (e.start_date IS NULL OR e.start_date > now())
	FROM events e WHERE e.id = $1`

// LockCapacity locks the Event's row in tx (FOR UPDATE) and reads its
// Capacity: ErrNotFound for no such Event. The active count is read after
// the lock, so it sees every Ticket a write that held the lock before made.
func LockCapacity(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (Capacity, error) {
	return readCapacity(ctx, tx, eventID, true)
}

// ReadCapacity is the Event's Capacity now, without a lock: for an answer
// that writes nothing.
func ReadCapacity(ctx context.Context, q pgxQuerier, eventID uuid.UUID) (Capacity, error) {
	return readCapacity(ctx, q, eventID, false)
}

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func readCapacity(ctx context.Context, q pgxQuerier, eventID uuid.UUID, lock bool) (Capacity, error) {
	query := capacitySQL
	if lock {
		query += ` FOR UPDATE OF e`
	}
	var c Capacity
	err := q.QueryRow(ctx, query, eventID).Scan(&c.Capacity, &c.Waitlist, &c.Promotable)
	if errors.Is(err, pgx.ErrNoRows) {
		return Capacity{}, ErrNotFound
	}
	if err != nil {
		return Capacity{}, err
	}
	err = q.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE event_id = $1 AND status = 'active'`, eventID).Scan(&c.Active)
	return c, err
}

// PromotionMail is what a Ticket promoted from the waiting list is told by.
type PromotionMail int

const (
	// PromotionNoMail queues nothing: neither mail is on.
	PromotionNoMail PromotionMail = iota
	// PromotionTicketCreated queues the "Biletin hazır" mail
	// (TICKET_CREATED_NOTIFICATION on, TICKET_STATUS_MAIL off), under the
	// Ticket's next resend number: its first key may have been used before
	// it was cancelled.
	PromotionTicketCreated
	// PromotionWaitlistPromoted queues "Yer açıldı, biletin hazır"
	// (event.waitlist-promoted; TICKET_STATUS_MAIL on).
	PromotionWaitlistPromoted
)

// PromotionMailFor is the promotion mail while the new Ticket's mail
// (created) and the ticket status mails (status) are on or off.
func PromotionMailFor(created, status bool) PromotionMail {
	switch {
	case status:
		return PromotionWaitlistPromoted
	case created:
		return PromotionTicketCreated
	}
	return PromotionNoMail
}

// QueuePromotedMail queues the mail of a Ticket just promoted, in tx.
func QueuePromotedMail(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID, mail PromotionMail) error {
	switch mail {
	case PromotionWaitlistPromoted:
		return QueueTicketMail(ctx, tx, ticketID, "promoted")
	case PromotionTicketCreated:
		var n int
		if err := tx.QueryRow(ctx, `
			INSERT INTO ticket_resends (ticket_id, n, renewed)
			SELECT $1, COALESCE(max(n), 0) + 1, false FROM ticket_resends WHERE ticket_id = $1
			RETURNING n`, ticketID).Scan(&n); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO ticket_notifications (ticket_id, resend, kind) VALUES ($1, $2, 'created')
			ON CONFLICT (ticket_id) DO UPDATE
			SET resend = EXCLUDED.resend, kind = EXCLUDED.kind, created_at = now(), next_attempt_at = now(), attempts = 0, claimed_at = NULL`, ticketID, n)
		return err
	}
	return nil
}

// QueueTicketMail queues a Ticket's mail of kind (waitlisted, promoted) in
// tx: a row still queued for the Ticket is replaced, so a "listedesin" not
// yet sent gives way to "yer açıldı".
func QueueTicketMail(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID, kind string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO ticket_notifications (ticket_id, kind) VALUES ($1, $2)
		ON CONFLICT (ticket_id) DO UPDATE
		SET kind = EXCLUDED.kind, resend = 0, created_at = now(), next_attempt_at = now(), attempts = 0, claimed_at = NULL`, ticketID, kind)
	return err
}

// PromoteWaitlist promotes the Event's waiting list in tx as far as its
// capacity lets: the longest waiting first, each queued its mail. Nothing
// moves while the Event is not promotable (a draft, cancelled, archived or
// started). It locks the Event's row first (a caller that holds it already
// keeps it) and answers the Tickets promoted; a count is logged, naming
// nobody.
func PromoteWaitlist(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, mail PromotionMail) ([]uuid.UUID, error) {
	c, err := LockCapacity(ctx, tx, eventID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	free := c.Free()
	if !c.Promotable || free == 0 {
		return nil, nil
	}
	// LIMIT NULL is no limit: an Event without a bound takes everyone.
	var limit *int
	if free > 0 {
		limit = &free
	}
	rows, err := tx.Query(ctx, `
		WITH next AS (
			SELECT id FROM tickets
			WHERE event_id = $1 AND status = 'waitlisted'
			ORDER BY waitlisted_at NULLS LAST, created_at, id
			LIMIT $2
			FOR UPDATE
		)
		UPDATE tickets t SET status = 'active', status_changed_at = now(), waitlisted_at = NULL, updated_at = now()
		FROM next WHERE t.id = next.id
		RETURNING t.id`, eventID, limit)
	if err != nil {
		return nil, err
	}
	promoted, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	for _, id := range promoted {
		if err := QueuePromotedMail(ctx, tx, id, mail); err != nil {
			return nil, err
		}
	}
	if len(promoted) > 0 {
		log.Printf("event %s: %d promoted from the waitlist", eventID, len(promoted))
	}
	return promoted, nil
}
