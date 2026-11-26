package raffle

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the Store on event_raffles and event_raffle_entries.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres is the Store on pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// liveSessionsSQL are the Event's ($1) live Sessions, as the certificate
// rule reads them: on a current day, neither archived nor cancelled.
const liveSessionsSQL = `
	SELECT s.id FROM sessions s
	JOIN event_days d ON d.id = s.event_day_id
	WHERE d.event_id = $1 AND d.archived_at IS NULL AND s.archived_at IS NULL AND NOT s.cancelled`

func (s *Postgres) Candidates(ctx context.Context, eventID uuid.UUID) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		WITH live AS (`+liveSessionsSQL+`),
		checked AS (
			SELECT ci.ticket_id, array_agg(DISTINCT ci.session_id) AS sessions
			FROM ticket_checkins ci
			JOIN live ON live.id = ci.session_id
			JOIN tickets t ON t.id = ci.ticket_id AND t.event_id = $1 AND t.status = 'active'
			GROUP BY ci.ticket_id
		),
		entered AS (
			SELECT v.ticket_id, array_agg(DISTINCT v.entry_date::text) AS dates
			FROM venue_entries v
			WHERE v.event_id = $1
			GROUP BY v.ticket_id
		)
		SELECT t.id, t.ticket_type, t.owner_id,
		       CASE WHEN t.ticket_type = 'GUEST'
		            THEN btrim(COALESCE(t.guest_first_name, '') || ' ' || COALESCE(t.guest_last_name, ''))
		            ELSE btrim(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, ''))
		       END,
		       lower(btrim(CASE WHEN t.ticket_type = 'GUEST' THEN COALESCE(t.guest_email, '') ELSE COALESCE(u.email, '') END)),
		       COALESCE(c.sessions, '{}'::uuid[]), COALESCE(e.dates, '{}'::text[])
		FROM tickets t
		LEFT JOIN users u ON u.id = t.owner_id
		LEFT JOIN checked c ON c.ticket_id = t.id
		LEFT JOIN entered e ON e.ticket_id = t.id
		WHERE t.event_id = $1 AND t.status = 'active'
		ORDER BY t.id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Candidate{}
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.TicketID, &c.TicketType, &c.OwnerID, &c.Name, &c.Email, &c.Sessions, &c.Entered); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// pastWinnersSQL are the Tickets holding a winner's place in the Event's
// ($1) raffles.
const pastWinnersSQL = `
	SELECT DISTINCT e.ticket_id FROM event_raffle_entries e
	JOIN event_raffles r ON r.id = e.raffle_id
	WHERE r.event_id = $1 AND e.status = 'winner'`

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func pastWinners(ctx context.Context, q querier, eventID uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := q.Query(ctx, pastWinnersSQL, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Postgres) PastWinners(ctx context.Context, eventID uuid.UUID) (map[uuid.UUID]bool, error) {
	return pastWinners(ctx, s.pool, eventID)
}

func (s *Postgres) Draw(ctx context.Context, eventID uuid.UUID, build func(map[uuid.UUID]bool) (Raffle, error)) (Raffle, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Raffle{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One draw of the Event at a time: the next reads the winners this one
	// keeps.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('event_raffle:' || $1::text, 0))`, eventID); err != nil {
		return Raffle{}, err
	}
	past, err := pastWinners(ctx, tx, eventID)
	if err != nil {
		return Raffle{}, err
	}
	r, err := build(past)
	if err != nil {
		return Raffle{}, err
	}
	criteria, err := json.Marshal(r.Criteria)
	if err != nil {
		return Raffle{}, err
	}
	var drawnBy *uuid.UUID
	if r.DrawnBy != nil {
		drawnBy = &r.DrawnBy.ID
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO event_raffles (id, event_id, title, criteria, eligible_ticket_ids, eligible_count,
			winner_count, alternate_count, list_hash, seed, algorithm, drawn_by, drawn_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		r.ID, r.EventID, r.Title, criteria, r.eligible, r.EligibleCount,
		r.WinnerCount, r.AlternateCount, r.ListHash, r.Seed, r.Algorithm, drawnBy, r.DrawnAt); err != nil {
		return Raffle{}, err
	}
	batch := &pgx.Batch{}
	for _, e := range r.Entries {
		batch.Queue(`INSERT INTO event_raffle_entries (raffle_id, position, ticket_id, status) VALUES ($1, $2, $3, $4)`,
			r.ID, e.Position, e.TicketID, e.Status)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return Raffle{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Raffle{}, err
	}
	return s.Get(ctx, eventID, r.ID)
}

const summaryCols = `r.id, r.title, r.criteria, r.eligible_count, r.winner_count, r.alternate_count, r.drawn_at,
	r.drawn_by, btrim(COALESCE(du.first_name, '') || ' ' || COALESCE(du.last_name, ''))`

func scanSummary(row pgx.Row, out *Summary) error {
	var criteria []byte
	var drawnBy *uuid.UUID
	var name string
	if err := row.Scan(&out.ID, &out.Title, &criteria, &out.EligibleCount, &out.WinnerCount, &out.AlternateCount,
		&out.DrawnAt, &drawnBy, &name); err != nil {
		return err
	}
	if err := json.Unmarshal(criteria, &out.Criteria); err != nil {
		return err
	}
	if out.Criteria.SessionIDs == nil {
		out.Criteria.SessionIDs = []uuid.UUID{}
	}
	if drawnBy != nil {
		out.DrawnBy = &Person{ID: *drawnBy, Name: name}
	}
	return nil
}

func (s *Postgres) List(ctx context.Context, eventID uuid.UUID) ([]Summary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+summaryCols+`
		FROM event_raffles r
		LEFT JOIN users du ON du.id = r.drawn_by
		WHERE r.event_id = $1
		ORDER BY r.drawn_at DESC, r.id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var sum Summary
		if err := scanSummary(rows, &sum); err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

func (s *Postgres) Get(ctx context.Context, eventID, raffleID uuid.UUID) (Raffle, error) {
	var sum Summary
	var r Raffle
	row := s.pool.QueryRow(ctx, `
		SELECT `+summaryCols+`, r.event_id, r.list_hash, r.seed, r.algorithm, r.eligible_ticket_ids
		FROM event_raffles r
		LEFT JOIN users du ON du.id = r.drawn_by
		WHERE r.id = $1 AND r.event_id = $2`, raffleID, eventID)
	var criteria []byte
	var drawnBy *uuid.UUID
	var name string
	err := row.Scan(&sum.ID, &sum.Title, &criteria, &sum.EligibleCount, &sum.WinnerCount, &sum.AlternateCount,
		&sum.DrawnAt, &drawnBy, &name, &r.EventID, &r.ListHash, &r.Seed, &r.Algorithm, &r.eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return Raffle{}, ErrNotFound
	}
	if err != nil {
		return Raffle{}, err
	}
	if err := json.Unmarshal(criteria, &sum.Criteria); err != nil {
		return Raffle{}, err
	}
	if sum.Criteria.SessionIDs == nil {
		sum.Criteria.SessionIDs = []uuid.UUID{}
	}
	if drawnBy != nil {
		sum.DrawnBy = &Person{ID: *drawnBy, Name: name}
	}
	r.ID, r.Title, r.Criteria, r.EligibleCount = sum.ID, sum.Title, sum.Criteria, sum.EligibleCount
	r.WinnerCount, r.AlternateCount, r.DrawnAt, r.DrawnBy = sum.WinnerCount, sum.AlternateCount, sum.DrawnAt, sum.DrawnBy
	rows, err := s.pool.Query(ctx, `
		SELECT e.position, e.status, e.ticket_id,
		       CASE WHEN t.ticket_type = 'GUEST'
		            THEN btrim(COALESCE(t.guest_first_name, '') || ' ' || COALESCE(t.guest_last_name, ''))
		            ELSE btrim(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, ''))
		       END,
		       CASE WHEN t.ticket_type = 'GUEST' THEN COALESCE(t.guest_email, '') ELSE COALESCE(u.email, '') END,
		       COALESCE(t.ticket_type, ''), e.promoted_at, e.forfeited_at, e.forfeit_reason, e.forfeited_by,
		       btrim(COALESCE(fu.first_name, '') || ' ' || COALESCE(fu.last_name, ''))
		FROM event_raffle_entries e
		LEFT JOIN tickets t ON t.id = e.ticket_id
		LEFT JOIN users u ON u.id = t.owner_id
		LEFT JOIN users fu ON fu.id = e.forfeited_by
		WHERE e.raffle_id = $1
		ORDER BY e.position`, raffleID)
	if err != nil {
		return Raffle{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var forfeitedBy *uuid.UUID
		var forfeiter string
		if err := rows.Scan(&e.Position, &e.Status, &e.TicketID, &e.Name, &e.email, &e.TicketType,
			&e.PromotedAt, &e.ForfeitedAt, &e.ForfeitReason, &forfeitedBy, &forfeiter); err != nil {
			return Raffle{}, err
		}
		if forfeitedBy != nil {
			e.ForfeitedBy = &Person{ID: *forfeitedBy, Name: forfeiter}
		}
		r.Entries = append(r.Entries, e)
	}
	return r, rows.Err()
}

func (s *Postgres) Forfeit(ctx context.Context, eventID, raffleID uuid.UUID, position int, reason string, by uuid.UUID, at time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var found bool
	if err := tx.QueryRow(ctx, `SELECT true FROM event_raffles WHERE id = $1 AND event_id = $2 FOR UPDATE`,
		raffleID, eventID).Scan(&found); errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	} else if err != nil {
		return 0, err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM event_raffle_entries WHERE raffle_id = $1 AND position = $2`,
		raffleID, position).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != StatusWinner) {
		return 0, ErrNotWinner
	}
	if err != nil {
		return 0, err
	}
	var next int
	err = tx.QueryRow(ctx, `
		SELECT position FROM event_raffle_entries
		WHERE raffle_id = $1 AND status = 'alternate'
		ORDER BY position LIMIT 1`, raffleID).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoAlternates
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE event_raffle_entries
		SET status = 'forfeited', forfeited_at = $3, forfeited_by = $4, forfeit_reason = $5
		WHERE raffle_id = $1 AND position = $2`, raffleID, position, at, by, reason); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE event_raffle_entries
		SET status = 'winner', promoted_at = $3, promoted_by = $4
		WHERE raffle_id = $1 AND position = $2`, raffleID, next, at, by); err != nil {
		return 0, err
	}
	return next, tx.Commit(ctx)
}
