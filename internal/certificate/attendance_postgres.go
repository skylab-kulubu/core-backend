package certificate

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// liveSessionsSQL are the Event's ($1) live Sessions: on a current day, not
// archived, not cancelled, with their two marks.
const liveSessionsSQL = `
	SELECT s.id, s.title, s.certificate_counted, s.certificate_required,
	       d.start_date, s.order_index, s.start_time
	FROM sessions s
	JOIN event_days d ON d.id = s.event_day_id
	WHERE d.event_id = $1 AND d.archived_at IS NULL
	  AND s.archived_at IS NULL AND NOT s.cancelled`

// EventAttendance reads the Event's attendance in two queries, however many
// Tickets it has: the live Sessions, then every Ticket with its counted
// check-ins, missing required Sessions, name, certificates and latest job
// (GROUP BY and LEFT JOINs; no query per Ticket).
func (s *PostgresStore) EventAttendance(ctx context.Context, eventID uuid.UUID) (Attendance, error) {
	out := Attendance{Required: []RequiredSession{}, Tickets: []TicketAttendance{}}
	rows, err := s.pool.Query(ctx, `
		SELECT live.id, live.title, live.certificate_counted, live.certificate_required
		FROM (`+liveSessionsSQL+`) live
		WHERE live.certificate_counted OR live.certificate_required
		ORDER BY live.start_date NULLS LAST, live.order_index, live.start_time NULLS LAST, live.id`, eventID)
	if err != nil {
		return Attendance{}, err
	}
	for rows.Next() {
		var id uuid.UUID
		var title string
		var counted, required bool
		if err := rows.Scan(&id, &title, &counted, &required); err != nil {
			rows.Close()
			return Attendance{}, err
		}
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
			       count(DISTINCT ci.session_id) FILTER (WHERE live.certificate_counted) AS counted,
			       array_agg(DISTINCT ci.session_id) FILTER (WHERE live.certificate_required) AS required
			FROM ticket_checkins ci
			JOIN tickets t ON t.id = ci.ticket_id AND t.event_id = $1
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
		       COALESCE(a.counted, 0),
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
		WHERE t.event_id = $1
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
		if err := rows.Scan(&row.TicketID, &row.TicketType, &row.Name, &row.Attended, &row.MissingRequired,
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
	return out, rows.Err()
}
