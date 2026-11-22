package dashboard

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The HomeStore half of PostgresStore: one query per method.

// currentSessions are the Sessions a schedule shows: not archived, not
// cancelled, of a day not archived.
const currentSessions = `
	FROM sessions s
	JOIN event_days d ON d.id = s.event_day_id
	WHERE d.event_id = ANY($1) AND d.archived_at IS NULL AND s.archived_at IS NULL AND NOT s.cancelled`

func (s *PostgresStore) SessionCounts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.event_id, count(*)`+currentSessions+` GROUP BY d.event_id`, eventIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]int)
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *PostgresStore) SessionsBetween(ctx context.Context, eventIDs []uuid.UUID, from, to time.Time, limit int) ([]SessionSlot, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id, d.event_id, s.title, s.start_time, s.end_time`+currentSessions+`
		AND s.start_time >= $2 AND s.start_time < $3
		ORDER BY s.start_time, s.order_index, s.id
		LIMIT $4`, eventIDs, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SessionSlot, 0)
	for rows.Next() {
		var slot SessionSlot
		if err := rows.Scan(&slot.ID, &slot.EventID, &slot.Title, &slot.StartTime, &slot.EndTime); err != nil {
			return nil, err
		}
		out = append(out, slot)
	}
	return out, rows.Err()
}

func (s *PostgresStore) FailedCertificateJobs(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT event_id, count(*) FROM certificate_jobs
		WHERE event_id = ANY($1) AND status = 'failed'
		GROUP BY event_id`, eventIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]int)
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *PostgresStore) OpenSubjectRequests(ctx context.Context, today string) (SubjectRequestCounts, error) {
	var out SubjectRequestCounts
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE due_on < $1::date)
		FROM subject_requests
		WHERE status IN ('received', 'awaiting_verification')`, today).Scan(&out.Open, &out.Overdue)
	return out, err
}

func (s *PostgresStore) DailyCheckIns(ctx context.Context, eventIDs []uuid.UUID, since time.Time, loc *time.Location) (map[uuid.UUID]map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.event_id, to_char(c.created_at AT TIME ZONE $3, 'YYYY-MM-DD') AS day, count(DISTINCT c.ticket_id)
		FROM ticket_checkins c
		JOIN tickets t ON t.id = c.ticket_id
		WHERE t.event_id = ANY($1) AND c.created_at >= $2
		GROUP BY t.event_id, day
	`, eventIDs, since, loc.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]map[string]int)
	for rows.Next() {
		var id uuid.UUID
		var day string
		var n int
		if err := rows.Scan(&id, &day, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = make(map[string]int)
		}
		out[id][day] = n
	}
	return out, rows.Err()
}

func (s *PostgresStore) CertificateCounts(ctx context.Context, eventIDs []uuid.UUID, from, mid time.Time) (CertificateCounts, error) {
	var out CertificateCounts
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE revoked_at IS NOT NULL),
		       count(*) FILTER (WHERE issued_at >= $3),
		       count(*) FILTER (WHERE issued_at >= $2 AND issued_at < $3)
		FROM certificates
		WHERE event_id = ANY($1)`, eventIDs, from, mid).Scan(&out.Issued, &out.Revoked, &out.Period.Current, &out.Period.Previous)
	return out, err
}
