package ticket

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// PageCheckIns is what the ticket page asks for while it is open on the
// Event's day (GET /v1/public/tickets/{key}/check-ins, docs/ticket-page.md):
// the Ticket the key opens, read in one query, so the page can notice its
// holder walking in. Nothing about anyone else, and no ids.
type PageCheckIns struct {
	// Held is a Ticket that still has its holder (Held) whose member
	// account, if it is a member's, is active: the page's own rule.
	Held bool
	// Event carries the Event's status and dates alone: what tells a
	// draft (no page) and a closed page (ticket.PageConfig.Closed).
	Event event.Event
	// CheckIns are the Ticket's own, oldest first.
	CheckIns []PageCheckIn
}

// PageCheckIn is one check-in as the ticket page shows it: the Session's
// title and when.
type PageCheckIn struct {
	Session string    `json:"session"`
	At      time.Time `json:"at"`
}

// PageCheckIns reads the Ticket key opens, its Event's status and dates and
// its check-ins with their Sessions' titles, in one query. ErrNotFound when
// no current Ticket has the key, or its Event is archived.
func (s *PostgresStore) PageCheckIns(ctx context.Context, key string) (PageCheckIns, error) {
	if !ValidAccessKey(key) {
		return PageCheckIns{}, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.ticket_type, t.owner_id, t.guest_email, u.account_state,
		       e.id, e.status, e.start_date, e.end_date,
		       s.title, c.created_at
		FROM tickets t
		JOIN events e ON e.id = t.event_id AND e.archived_at IS NULL
		LEFT JOIN users u ON u.id = t.owner_id
		LEFT JOIN ticket_checkins c ON c.ticket_id = t.id
		LEFT JOIN sessions s ON s.id = c.session_id
		WHERE t.access_key = $1
		ORDER BY c.created_at
	`, key)
	if err != nil {
		return PageCheckIns{}, err
	}
	defer rows.Close()
	out := PageCheckIns{CheckIns: []PageCheckIn{}}
	found := false
	for rows.Next() {
		var (
			t            Ticket
			accountState *string
			status       string
			title        *string
			at           *time.Time
			eventID      uuid.UUID
		)
		if err := rows.Scan(&t.TicketType, &t.OwnerID, &t.GuestEmail, &accountState,
			&eventID, &status, &out.Event.StartDate, &out.Event.EndDate, &title, &at); err != nil {
			return PageCheckIns{}, err
		}
		if !found {
			found = true
			out.Event.ID = eventID
			out.Event.Status = event.Status(status)
			out.Held = held(t) && (t.TicketType == Guest || (accountState != nil && *accountState == "active"))
		}
		if at != nil {
			check := PageCheckIn{At: *at}
			if title != nil {
				check.Session = *title
			}
			out.CheckIns = append(out.CheckIns, check)
		}
	}
	if err := rows.Err(); err != nil {
		return PageCheckIns{}, err
	}
	if !found {
		return PageCheckIns{}, ErrNotFound
	}
	return out, nil
}
