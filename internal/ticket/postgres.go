package ticket

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const ticketCols = `id, event_id, ticket_type, owner_id, guest_first_name, guest_last_name, guest_email, guest_phone_number, created_at, updated_at`

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
		checkIns, err := s.checkInsFor(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		t.CheckIns = checkIns
		out = append(out, t)
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
		SELECT COUNT(1) FROM tickets WHERE guest_email = $1 AND event_id = $2
	`, email, eventID).Scan(&n)
	return n > 0, err
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
		INSERT INTO ticket_checkins (id, ticket_id, event_day_id)
		VALUES ($1, $2, $3)
		RETURNING id, ticket_id, event_day_id, created_at
	`, c.ID, c.TicketID, c.EventDayID).Scan(&c.ID, &c.TicketID, &c.EventDayID, &c.CreatedAt)
	return c, err
}

func (s *PostgresStore) HasCheckIn(ctx context.Context, ticketID, eventDayID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1) FROM ticket_checkins WHERE ticket_id = $1 AND event_day_id = $2
	`, ticketID, eventDayID).Scan(&n)
	return n > 0, err
}

func (s *PostgresStore) checkInsFor(ctx context.Context, ticketID uuid.UUID) ([]CheckIn, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, ticket_id, event_day_id, created_at
		FROM ticket_checkins WHERE ticket_id = $1 ORDER BY created_at
	`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CheckIn, 0)
	for rows.Next() {
		var c CheckIn
		if err := rows.Scan(&c.ID, &c.TicketID, &c.EventDayID, &c.CreatedAt); err != nil {
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
