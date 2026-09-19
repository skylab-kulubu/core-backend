package ticket

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const ticketCols = `id, event_id, ticket_type, owner_id, guest_first_name, guest_last_name, guest_email, guest_phone_number, created_at, updated_at`
const doorTicketCols = `t.id, t.event_id, t.ticket_type, t.owner_id`

const doorNameSQL = `CASE
	WHEN t.ticket_type = 'GUEST' THEN COALESCE(
		NULLIF(trim(concat_ws(' ', t.guest_first_name, t.guest_last_name)), ''),
		NULLIF(trim(t.guest_email), ''),
		''
	)
	ELSE COALESCE(
		NULLIF(trim(concat_ws(' ', u.first_name, u.last_name)), ''),
		NULLIF(trim(u.email), ''),
		t.owner_id::text,
		''
	)
END`

const doorEmailSQL = `CASE
	WHEN t.ticket_type = 'GUEST' THEN t.guest_email
	ELSE COALESCE(u.email, '')
END`

func (s *PostgresStore) Create(ctx context.Context, t Ticket) (Ticket, error) {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO tickets (
			id, event_id, ticket_type, owner_id,
			guest_first_name, guest_last_name, guest_email, guest_phone_number
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING `+ticketCols,
		t.ID, t.EventID, t.TicketType, t.OwnerID,
		t.GuestFirstName, t.GuestLastName, t.GuestEmail, t.GuestPhoneNumber)
	got, err := scanTicket(row)
	if err != nil {
		return Ticket{}, err
	}
	got.CheckIns = []CheckIn{}
	return got, nil
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	checkIns, err := s.checkInsFor(ctx, t.ID)
	if err != nil {
		return Ticket{}, err
	}
	t.CheckIns = checkIns
	return t, nil
}

func (s *PostgresStore) ListByOwner(ctx context.Context, ownerID uuid.UUID) ([]Ticket, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets WHERE owner_id = $1 ORDER BY created_at`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Ticket, 0)
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		checkIns, err := s.checkInsFor(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		t.CheckIns = checkIns
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Ticket, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets WHERE event_id = $1 ORDER BY created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Ticket, 0)
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return s.withCheckIns(ctx, out)
}

func (s *PostgresStore) withCheckIns(ctx context.Context, tickets []Ticket) ([]Ticket, error) {
	if len(tickets) == 0 {
		return tickets, nil
	}
	ids := make([]uuid.UUID, len(tickets))
	byID := make(map[uuid.UUID]int, len(tickets))
	for i := range tickets {
		ids[i] = tickets[i].ID
		byID[tickets[i].ID] = i
		tickets[i].CheckIns = []CheckIn{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, ticket_id, event_day_id, session_id, created_at
		FROM ticket_checkins
		WHERE ticket_id = ANY($1)
		ORDER BY created_at
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c CheckIn
		if err := rows.Scan(&c.ID, &c.TicketID, &c.EventDayID, &c.SessionID, &c.CreatedAt); err != nil {
			return nil, err
		}
		if index, ok := byID[c.TicketID]; ok {
			tickets[index].CheckIns = append(tickets[index].CheckIns, c)
		}
	}
	return tickets, rows.Err()
}

func (s *PostgresStore) SearchDoorTickets(
	ctx context.Context,
	eventID uuid.UUID,
	query string,
	exact bool,
	limit int,
) ([]DoorTicketIdentity, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+doorTicketCols+`, `+doorNameSQL+`, `+doorEmailSQL+`,
		       (t.ticket_type = 'GUEST' OR u.id IS NOT NULL)
		FROM tickets t
		LEFT JOIN users u ON u.id = t.owner_id
		WHERE t.event_id = $1
		  AND (t.ticket_type = 'GUEST' OR u.id IS NOT NULL)
		  AND CASE WHEN $3 THEN (
			lower(trim(`+doorNameSQL+`)) = lower(trim($2))
			OR lower(trim(`+doorEmailSQL+`)) = lower(trim($2))
			OR t.owner_id::text = trim($2)
		  ) ELSE (
			lower(`+doorNameSQL+`) LIKE '%' || lower(trim($2)) || '%'
			OR lower(`+doorEmailSQL+`) LIKE '%' || lower(trim($2)) || '%'
			OR t.owner_id::text LIKE '%' || trim($2) || '%'
		  ) END
		ORDER BY lower(`+doorNameSQL+`), lower(`+doorEmailSQL+`), t.created_at
		LIMIT $4
	`, eventID, query, exact, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDoorTicketIdentities(rows)
}

func (s *PostgresStore) DoorTicketsByOwners(
	ctx context.Context,
	eventID uuid.UUID,
	ownerIDs []uuid.UUID,
) ([]DoorTicketIdentity, error) {
	if len(ownerIDs) == 0 {
		return []DoorTicketIdentity{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+doorTicketCols+`, `+doorNameSQL+`, `+doorEmailSQL+`, (u.id IS NOT NULL)
		FROM tickets t
		LEFT JOIN users u ON u.id = t.owner_id
		WHERE t.event_id = $1 AND t.owner_id = ANY($2)
		ORDER BY t.created_at
	`, eventID, ownerIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDoorTicketIdentities(rows)
}

func (s *PostgresStore) DoorSessionActivity(
	ctx context.Context,
	sessionID uuid.UUID,
	limit int,
) (DoorActivity, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.ticket_id, c.event_day_id, c.session_id, c.created_at,
		       `+doorNameSQL+`, count(*) OVER()
		FROM ticket_checkins c
		JOIN tickets t ON t.id = c.ticket_id
		LEFT JOIN users u ON u.id = t.owner_id
		WHERE c.session_id = $1
		ORDER BY c.created_at DESC
		LIMIT $2
	`, sessionID, limit)
	if err != nil {
		return DoorActivity{}, err
	}
	defer rows.Close()
	activity := DoorActivity{Items: []DoorCheckIn{}}
	for rows.Next() {
		var item DoorCheckIn
		if err := rows.Scan(
			&item.ID, &item.TicketID, &item.EventDayID, &item.SessionID, &item.CreatedAt,
			&item.PersonName, &activity.Total,
		); err != nil {
			return DoorActivity{}, err
		}
		activity.Items = append(activity.Items, item)
	}
	return activity, rows.Err()
}

func scanDoorTicketIdentities(rows pgx.Rows) ([]DoorTicketIdentity, error) {
	out := make([]DoorTicketIdentity, 0)
	for rows.Next() {
		var row DoorTicketIdentity
		if err := rows.Scan(
			&row.Ticket.ID, &row.Ticket.EventID, &row.Ticket.TicketType, &row.Ticket.OwnerID,
			&row.Name, &row.Email, &row.Found,
		); err != nil {
			return nil, err
		}
		row.Ticket.CheckIns = []CheckIn{}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ExistsOwnerEvent(ctx context.Context, ownerID, eventID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1) FROM tickets WHERE owner_id = $1 AND event_id = $2
	`, ownerID, eventID).Scan(&n)
	return n > 0, err
}

func (s *PostgresStore) ExistsGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1) FROM tickets WHERE lower(guest_email) = lower($1) AND event_id = $2
	`, email, eventID).Scan(&n)
	return n > 0, err
}

func (s *PostgresStore) GetByGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx, `
		SELECT `+ticketCols+` FROM tickets
		WHERE ticket_type = 'GUEST' AND lower(guest_email) = lower($1) AND event_id = $2
	`, email, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	checkIns, err := s.checkInsFor(ctx, t.ID)
	if err != nil {
		return Ticket{}, err
	}
	t.CheckIns = checkIns
	return t, nil
}

func (s *PostgresStore) Update(ctx context.Context, t Ticket) (Ticket, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE tickets SET
			guest_first_name = $1,
			guest_last_name = $2,
			guest_email = $3,
			guest_phone_number = $4,
			updated_at = now()
		WHERE id = $5
		RETURNING `+ticketCols,
		t.GuestFirstName, t.GuestLastName, t.GuestEmail, t.GuestPhoneNumber, t.ID)
	got, err := scanTicket(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	checkIns, err := s.checkInsFor(ctx, got.ID)
	if err != nil {
		return Ticket{}, err
	}
	got.CheckIns = checkIns
	return got, nil
}

func (s *PostgresStore) GetByOwnerEvent(ctx context.Context, ownerID, eventID uuid.UUID) (Ticket, error) {
	t, err := scanTicket(s.pool.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE owner_id = $1 AND event_id = $2`, ownerID, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	checkIns, err := s.checkInsFor(ctx, t.ID)
	if err != nil {
		return Ticket{}, err
	}
	t.CheckIns = checkIns
	return t, nil
}

func (s *PostgresStore) ListByGuestEmail(ctx context.Context, email string) ([]Ticket, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets WHERE lower(guest_email) = lower($1) ORDER BY created_at`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Ticket, 0)
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		checkIns, err := s.checkInsFor(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		t.CheckIns = checkIns
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AddCheckIn(ctx context.Context, c CheckIn) (CheckIn, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id, ticket_id, event_day_id, session_id, created_at
	`, c.ID, c.TicketID, c.EventDayID, c.SessionID).Scan(&c.ID, &c.TicketID, &c.EventDayID, &c.SessionID, &c.CreatedAt)
	if isUnique(err) {
		return CheckIn{}, ErrConflict
	}
	return c, err
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func (s *PostgresStore) HasCheckIn(ctx context.Context, ticketID, sessionID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1) FROM ticket_checkins WHERE ticket_id = $1 AND session_id = $2
	`, ticketID, sessionID).Scan(&n)
	return n > 0, err
}

func (s *PostgresStore) checkInsFor(ctx context.Context, ticketID uuid.UUID) ([]CheckIn, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, ticket_id, event_day_id, session_id, created_at
		FROM ticket_checkins WHERE ticket_id = $1 ORDER BY created_at
	`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CheckIn, 0)
	for rows.Next() {
		var c CheckIn
		if err := rows.Scan(&c.ID, &c.TicketID, &c.EventDayID, &c.SessionID, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTicket(row rowScanner) (Ticket, error) {
	var t Ticket
	var created, updated time.Time
	err := row.Scan(
		&t.ID, &t.EventID, &t.TicketType, &t.OwnerID,
		&t.GuestFirstName, &t.GuestLastName, &t.GuestEmail, &t.GuestPhoneNumber,
		&created, &updated,
	)
	t.CreatedAt = created
	t.UpdatedAt = updated
	return t, err
}
