package certificate

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// eventEndsSQL is an Event's (e) end, the feedback mail's expression: its
// end date, else a day after its start; NULL when it has neither.
const eventEndsSQL = `COALESCE(e.end_date, e.start_date + interval '24 hours')`

// sessionEndsSQL is a Session's (s, on day d of Event e) end, the feedback
// mail's expression: its end time, else its day's end, else its Event's.
const sessionEndsSQL = `COALESCE(s.end_time, d.end_date, ` + eventEndsSQL + `)`

// programMinutesSQL is a Session's (s) program length in whole minutes, as
// programMinutes reads it: 0 without both times or when it ends before it
// starts.
const programMinutesSQL = `COALESCE(GREATEST(0, floor(extract(epoch FROM s.end_time - s.start_time) / 60)), 0)::int`

// liveSessionsSQL are the Event's ($1) live Sessions: on a current day, not
// archived, not cancelled, with their two marks and their end.
const liveSessionsSQL = `
	SELECT s.id, s.title, s.certificate_counted, s.certificate_required,
	       d.start_date, s.order_index, s.start_time, ` + sessionEndsSQL + ` AS ends_at,
	       ` + programMinutesSQL + ` AS program_minutes
	FROM sessions s
	JOIN event_days d ON d.id = s.event_day_id
	JOIN events e ON e.id = d.event_id
	WHERE d.event_id = $1 AND d.archived_at IS NULL
	  AND s.archived_at IS NULL AND NOT s.cancelled`

// EventAttendance reads the Event's attendance in two queries, however many
// Tickets it has (active ones only, docs/ticket-cancellation.md: a cancelled
// or waitlisted Ticket is neither eligible, finalized, rescanned nor
// listed): the live Sessions, then every Ticket with its counted
// check-ins, missing required Sessions, name, certificates and latest job
// (GROUP BY and LEFT JOINs; no query per Ticket).
func (s *PostgresStore) EventAttendance(ctx context.Context, eventID uuid.UUID) (Attendance, error) {
	out := Attendance{Required: []RequiredSession{}, Tickets: []TicketAttendance{}}
	rows, err := s.pool.Query(ctx, `
		SELECT live.id, live.title, live.certificate_counted, live.certificate_required, live.ends_at
		FROM (`+liveSessionsSQL+`) live
		ORDER BY live.start_date NULLS LAST, live.order_index, live.start_time NULLS LAST, live.id`, eventID)
	if err != nil {
		return Attendance{}, err
	}
	for rows.Next() {
		var id uuid.UUID
		var title string
		var counted, required bool
		var ends *time.Time
		if err := rows.Scan(&id, &title, &counted, &required, &ends); err != nil {
			rows.Close()
			return Attendance{}, err
		}
		out.addLiveSession(ends)
		if counted {
			out.CountedTotal++
		}
		if required {
			out.Required = append(out.Required, RequiredSession{ID: id, Title: title})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Attendance{}, err
	}

	rows, err = s.pool.Query(ctx, `
		WITH live AS (`+liveSessionsSQL+`),
		attended AS (
			SELECT ci.ticket_id,
			       count(*) FILTER (WHERE live.certificate_counted) AS counted,
			       COALESCE(sum(live.program_minutes) FILTER (WHERE live.certificate_counted), 0)::int AS minutes,
			       array_agg(ci.session_id) FILTER (WHERE live.certificate_required) AS required
			FROM (
				SELECT DISTINCT ci.ticket_id, ci.session_id
				FROM ticket_checkins ci
				JOIN tickets t ON t.id = ci.ticket_id AND t.event_id = $1 AND t.status = 'active'
			) ci
			JOIN live ON live.id = ci.session_id
			GROUP BY ci.ticket_id
		),
		latest_job AS (
			SELECT DISTINCT ON (ticket_id) ticket_id, status
			FROM certificate_jobs
			WHERE event_id = $1
			ORDER BY ticket_id, created_at DESC,
			         CASE status WHEN 'queued' THEN 0 WHEN 'running' THEN 0 WHEN 'failed' THEN 1 ELSE 2 END
		),
		latest_revoked AS (
			SELECT DISTINCT ON (ticket_id) ticket_id, serial, issued_at, revoked_at
			FROM certificates
			WHERE event_id = $1 AND revoked_at IS NOT NULL
			ORDER BY ticket_id, revoked_at DESC
		)
		SELECT t.id, t.ticket_type,
		       CASE WHEN t.ticket_type = 'GUEST'
		            THEN btrim(t.guest_first_name || ' ' || t.guest_last_name)
		            ELSE btrim(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, ''))
		       END,
		       COALESCE(a.counted, 0), COALESCE(a.minutes, 0),
		       ARRAY(SELECT live.id FROM live
		             WHERE live.certificate_required AND NOT (live.id = ANY(COALESCE(a.required, '{}'::uuid[])))
		             ORDER BY live.id),
		       c.serial, c.issued_at,
		       r.serial, r.issued_at, r.revoked_at,
		       COALESCE(j.status, '')
		FROM tickets t
		LEFT JOIN users u ON u.id = t.owner_id
		LEFT JOIN attended a ON a.ticket_id = t.id
		LEFT JOIN certificates c ON c.event_id = $1 AND c.ticket_id = t.id AND c.revoked_at IS NULL
		LEFT JOIN latest_revoked r ON r.ticket_id = t.id AND c.id IS NULL
		LEFT JOIN latest_job j ON j.ticket_id = t.id
		WHERE t.event_id = $1 AND t.status = 'active'
		ORDER BY t.created_at, t.id`, eventID)
	if err != nil {
		return Attendance{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var row TicketAttendance
		var activeSerial, revokedSerial *string
		var activeIssued, revokedIssued, revokedAt *time.Time
		var job string
		if err := rows.Scan(&row.TicketID, &row.TicketType, &row.Name, &row.Attended, &row.ProgramMinutes, &row.MissingRequired,
			&activeSerial, &activeIssued, &revokedSerial, &revokedIssued, &revokedAt, &job); err != nil {
			return Attendance{}, err
		}
		if row.MissingRequired == nil {
			row.MissingRequired = []uuid.UUID{}
		}
		if activeSerial != nil && activeIssued != nil {
			row.Active = &CertificateRef{Serial: *activeSerial, IssuedAt: *activeIssued}
		}
		if revokedSerial != nil && revokedIssued != nil {
			row.Revoked = &CertificateRef{Serial: *revokedSerial, IssuedAt: *revokedIssued, RevokedAt: revokedAt}
		}
		switch job {
		case "queued", "running":
			row.Job = JobQueued
		case "failed":
			row.Job = JobFailed
		}
		out.Tickets = append(out.Tickets, row)
	}
	if err := rows.Err(); err != nil {
		return Attendance{}, err
	}
	rows.Close()
	if out.LiveSessions > 0 {
		return out, nil
	}
	return s.venueCounted(ctx, eventID, out)
}

// venueCounted reads, for an Event with no live Session, its end and the
// active Tickets with a Venue entry: a third query, only then.
func (s *PostgresStore) venueCounted(ctx context.Context, eventID uuid.UUID, out Attendance) (Attendance, error) {
	var ends *time.Time
	var entered []uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT `+eventEndsSQL+`,
		       ARRAY(SELECT DISTINCT v.ticket_id FROM venue_entries v
		             JOIN tickets t ON t.id = v.ticket_id AND t.status = 'active'
		             WHERE v.event_id = e.id)
		FROM events e WHERE e.id = $1`, eventID).Scan(&ends, &entered)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return Attendance{}, err
	}
	set := make(map[uuid.UUID]bool, len(entered))
	for _, id := range entered {
		set[id] = true
	}
	out.venueCounted(set, ends)
	return out, nil
}
