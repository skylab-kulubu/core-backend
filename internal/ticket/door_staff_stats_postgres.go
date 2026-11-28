package ticket

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// doorMarksSQL is every mark that stands at Event $1's door, as (kind,
// source, by, at): its Venue entries, and its Session Check-ins. A
// Check-in written before ticket_checkins kept its source and writer
// (20261128120000) takes them from the Venue entry it wrote in the same
// statement (same created_at): attendance is the person's own, desk the
// desk's with its door staff; a scan's names no Check-in, and a Check-in
// whose entry was handed to it keeps none. at is when the door took it.
const doorMarksSQL = `
	SELECT 'entry' AS kind, v.source, v.by_user_id AS by, COALESCE(v.scanned_at, v.created_at) AS at
	FROM venue_entries v
	WHERE v.event_id = $1
	UNION ALL
	SELECT 'check_in', COALESCE(c.source, CASE ve.source WHEN 'attendance' THEN 'self' WHEN 'desk' THEN 'desk' END),
		COALESCE(c.by_user_id, CASE WHEN c.source IS NULL THEN ve.by_user_id END), COALESCE(c.scanned_at, c.created_at)
	FROM ticket_checkins c
	JOIN tickets t ON t.id = c.ticket_id
	LEFT JOIN venue_entries ve ON ve.check_in_id = c.id AND ve.created_at = c.created_at
	WHERE t.event_id = $1`

func (s *PostgresStore) DoorStats(ctx context.Context, eventID uuid.UUID) (DoorStatsData, error) {
	out := DoorStatsData{Hours: map[uuid.UUID][]DoorHour{}}
	rows, err := s.pool.Query(ctx, `
		WITH marks AS (`+doorMarksSQL+`)
		SELECT kind, COALESCE(source, ''), by, count(*), min(at), max(at)
		FROM marks GROUP BY kind, source, by`, eventID)
	if err != nil {
		return DoorStatsData{}, err
	}
	for rows.Next() {
		var m DoorMarkCount
		if err := rows.Scan(&m.Kind, &m.Source, &m.By, &m.Count, &m.FirstAt, &m.LastAt); err != nil {
			rows.Close()
			return DoorStatsData{}, err
		}
		out.Marks = append(out.Marks, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DoorStatsData{}, err
	}

	// The marks of each named writer by Istanbul hour, in order. The
	// people's own carry no writer.
	rows, err = s.pool.Query(ctx, `
		WITH marks AS (`+doorMarksSQL+`)
		SELECT by, date_trunc('hour', at AT TIME ZONE 'Europe/Istanbul') AS hour, count(*)
		FROM marks WHERE by IS NOT NULL
		GROUP BY by, hour ORDER BY by, hour`, eventID)
	if err != nil {
		return DoorStatsData{}, err
	}
	for rows.Next() {
		var by uuid.UUID
		var wall time.Time
		var n int
		if err := rows.Scan(&by, &wall, &n); err != nil {
			rows.Close()
			return DoorStatsData{}, err
		}
		hour := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), 0, 0, 0, istanbul)
		out.Hours[by] = append(out.Hours[by], DoorHour{Hour: hour, Count: n})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DoorStatsData{}, err
	}

	// Walk-ins: the Team audit rows of the Tickets the door wrote (ok),
	// by who wrote them (docs/walk-in.md).
	if out.WalkIns, err = s.personCounts(ctx, `
		SELECT actor_id, count(*), min(occurred_at), max(occurred_at)
		FROM team_audit_records
		WHERE action = 'ticket.walk_in' AND outcome = 'ok' AND after->>'eventId' = $1::text
		GROUP BY actor_id`, eventID); err != nil {
		return DoorStatsData{}, err
	}
	if out.Undone, err = s.personCounts(ctx, `
		SELECT by_user_id, count(*), min(undone_at), max(undone_at)
		FROM door_undos WHERE event_id = $1 AND by_user_id IS NOT NULL
		GROUP BY by_user_id`, eventID); err != nil {
		return DoorStatsData{}, err
	}
	if out.Undid, err = s.personCounts(ctx, `
		SELECT undone_by, count(*), min(undone_at), max(undone_at)
		FROM door_undos WHERE event_id = $1 AND undone_by IS NOT NULL
		GROUP BY undone_by`, eventID); err != nil {
		return DoorStatsData{}, err
	}
	return out, nil
}

func (s *PostgresStore) personCounts(ctx context.Context, query string, args ...any) ([]DoorPersonCount, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DoorPersonCount{}
	for rows.Next() {
		var c DoorPersonCount
		if err := rows.Scan(&c.UserID, &c.Count, &c.FirstAt, &c.LastAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
