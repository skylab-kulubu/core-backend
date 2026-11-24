package ticket

import (
	"context"

	"github.com/google/uuid"
)

// ImportMatches reads, for every e-mail of an attendee list in one query,
// the active accounts that hold it and the Event's Ticket it holds as a
// guest or through one of those accounts (docs/attendee-import.md).
func (s *PostgresStore) ImportMatches(ctx context.Context, eventID uuid.UUID, emails []string) (map[string]ImportMatch, error) {
	rows, err := s.pool.Query(ctx, `
		WITH input AS (
			SELECT DISTINCT lower(btrim(x)) AS email FROM unnest($2::text[]) AS x
		), accounts AS (
			SELECT i.email, u.id
			FROM input i JOIN users u ON u.account_state = 'active' AND lower(u.email) = i.email
		)
		SELECT i.email,
			COALESCE((SELECT array_agg(a.id ORDER BY a.id) FROM accounts a WHERE a.email = i.email), '{}'::uuid[]),
			COALESCE((
				SELECT t.status FROM tickets t
				WHERE t.event_id = $1
				  AND ((t.ticket_type = 'GUEST' AND lower(t.guest_email) = i.email)
				    OR t.owner_id IN (SELECT a.id FROM accounts a WHERE a.email = i.email))
				ORDER BY CASE t.status WHEN 'active' THEN 0 WHEN 'waitlisted' THEN 1 ELSE 2 END
				LIMIT 1), '')
		FROM input i`, eventID, emails)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]ImportMatch, len(emails))
	for rows.Next() {
		var (
			email    string
			accounts []uuid.UUID
			status   string
		)
		if err := rows.Scan(&email, &accounts, &status); err != nil {
			return nil, err
		}
		out[email] = ImportMatch{Accounts: accounts, Ticket: Status(status)}
	}
	return out, rows.Err()
}

var _ ImportMatcher = (*PostgresStore)(nil)
