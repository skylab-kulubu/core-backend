package dashboard

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// PostgresStore answers Store with one aggregate query per method, however
// many Events or people it is asked about.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// TicketCounts reads every check-in a Ticket has, as the applicant list
// does: archiving an EventDay or a Session keeps its check-ins.
func (s *PostgresStore) TicketCounts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]TicketCounts, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.event_id,
		       count(*),
		       count(*) FILTER (WHERE t.ticket_type = $2),
		       count(*) FILTER (WHERE t.ticket_type <> $2),
		       count(*) FILTER (WHERE EXISTS (SELECT 1 FROM ticket_checkins c WHERE c.ticket_id = t.id))
		FROM tickets t
		WHERE t.event_id = ANY($1)
		GROUP BY t.event_id
	`, eventIDs, ticket.Registered)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]TicketCounts)
	for rows.Next() {
		var id uuid.UUID
		var c TicketCounts
		if err := rows.Scan(&id, &c.Applications, &c.Members, &c.Guests, &c.CheckedIn); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

func (s *PostgresStore) DailyApplications(ctx context.Context, eventIDs []uuid.UUID, since time.Time, loc *time.Location) (map[uuid.UUID]map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.event_id, to_char(t.created_at AT TIME ZONE $3, 'YYYY-MM-DD') AS day, count(*)
		FROM tickets t
		WHERE t.event_id = ANY($1) AND t.created_at >= $2
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

// Accounts reads the people's rows and deletion markers in one query.
func (s *PostgresStore) Accounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Account, error) {
	out := make(map[uuid.UUID]Account)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ids.id,
		       u.id IS NOT NULL,
		       COALESCE(u.first_name, ''),
		       COALESCE(u.last_name, ''),
		       COALESCE(u.account_state <> 'active', false)
		         OR EXISTS (SELECT 1 FROM account_deletion_requests r WHERE r.subject_id = ids.id)
		FROM unnest($1::uuid[]) AS ids(id)
		LEFT JOIN users u ON u.id = ids.id
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var a Account
		if err := rows.Scan(&id, &a.Stored, &a.FirstName, &a.LastName, &a.Blocked); err != nil {
			return nil, err
		}
		if a.Stored || a.Blocked {
			out[id] = a
		}
	}
	return out, rows.Err()
}
