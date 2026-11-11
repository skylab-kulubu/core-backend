package feedback

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TicketLink is one of a Ticket's feedback links as its holder's own
// tickets answer (`GET /v1/tickets/me`, sky-app; ticket 12): the link the
// mail sent them (the same invite code), while the form still takes it.
type TicketLink struct {
	// URL is the form's address with the invite code (Link).
	URL string `json:"url"`
	// Title is the Session's title, or the Event's name for the Event's
	// form: the mail's item title.
	Title string `json:"title"`
	// SessionID is the Session whose form it is; null for the Event's.
	SessionID *uuid.UUID `json:"sessionId"`
	// Answered reports whether Forms reported an answer for the invite.
	Answered bool `json:"answered"`
}

// TicketLinks are the Tickets' feedback links, by Ticket, in one query: one
// for each invite (a feedback mail went with it) whose target still takes
// it as the mail's worker would read it now. Left out: an Event no longer
// published or archived, a Session cancelled or archived (or its day), a
// Session's form once the Event left the per_session mode, a target whose
// form changed since (the code is the old form's), and a form
// that closed (its invites' reminders were cancelled, or its closing time
// has come by now; as the reminder, opening it again does not bring the
// link back). An answered invite stays, Answered. A Ticket with none is
// not in the map. The Event's form comes after its Sessions', the Sessions
// in their order (the mail's).
func (s *PostgresStore) TicketLinks(ctx context.Context, ticketIDs []uuid.UUID, now time.Time) (map[uuid.UUID][]TicketLink, error) {
	if len(ticketIDs) == 0 {
		return map[uuid.UUID][]TicketLink{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT fi.ticket_id, fi.session_id, fi.form_id, fi.code, fi.answered_at IS NOT NULL,
			COALESCE(s.feedback_form_url, e.feedback_form_url),
			COALESCE(s.title, e.name)
		FROM feedback_invites fi
		JOIN events e ON e.id = fi.event_id
		LEFT JOIN sessions s ON s.id = fi.session_id
		LEFT JOIN event_days d ON d.id = s.event_day_id
		WHERE fi.ticket_id = ANY($1)
		  AND e.status = 'published' AND e.archived_at IS NULL
		  AND (fi.session_id IS NULL OR (e.feedback_mode = 'per_session' AND s.archived_at IS NULL AND NOT s.cancelled AND d.archived_at IS NULL))
		  AND fi.reminder_cancelled_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM feedback_form_closings c WHERE c.form_id = fi.form_id AND c.closes_at <= $2)
		ORDER BY fi.ticket_id, fi.session_id IS NULL, s.start_time NULLS LAST, s.order_index, fi.session_id`,
		ticketIDs, now)
	if err != nil {
		return nil, err
	}
	type row struct {
		ticketID, formID uuid.UUID
		sessionID        *uuid.UUID
		code, formURL    string
		title            string
		answered         bool
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var v row
		err := r.Scan(&v.ticketID, &v.sessionID, &v.formID, &v.code, &v.answered, &v.formURL, &v.title)
		return v, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]TicketLink)
	for _, v := range all {
		if id, ok := formIDFromURL(v.formURL); !ok || id != v.formID {
			continue
		}
		link, err := Link(v.formURL, v.code)
		if err != nil {
			continue
		}
		out[v.ticketID] = append(out[v.ticketID], TicketLink{URL: link, Title: v.title, SessionID: v.sessionID, Answered: v.answered})
	}
	return out, nil
}
