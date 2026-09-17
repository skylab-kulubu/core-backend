package certificate

import (
	"context"
	"errors"

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

const certCols = `id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, revoked_at, issued_at`

func (s *PostgresStore) Create(ctx context.Context, c Certificate, pdf []byte) (Certificate, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO certificates (
			id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email,
			event_name, owner_team, verify_url, pdf, issued_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now())
		RETURNING `+certCols,
		c.ID, c.EventID, c.TicketID, c.OwnerID, c.Serial, c.RecipientName, c.RecipientEmail,
		c.EventName, c.OwnerTeam, c.VerifyURL, pdf,
	).Scan(
		&c.ID, &c.EventID, &c.TicketID, &c.OwnerID, &c.Serial, &c.RecipientName, &c.RecipientEmail,
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.RevokedAt, &c.IssuedAt,
	)
	if isUnique(err) {
		return Certificate{}, ErrConflict
	}
	return c, err
}

func (s *PostgresStore) GetBySerial(ctx context.Context, serial string) (Certificate, []byte, error) {
	var c Certificate
	var pdf []byte
	err := s.pool.QueryRow(ctx, `SELECT `+certCols+`, pdf FROM certificates WHERE serial = $1`, serial).Scan(
		&c.ID, &c.EventID, &c.TicketID, &c.OwnerID, &c.Serial, &c.RecipientName, &c.RecipientEmail,
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.RevokedAt, &c.IssuedAt, &pdf,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, nil, ErrNotFound
	}
	return c, pdf, err
}

func (s *PostgresStore) GetActive(ctx context.Context, eventID, ticketID uuid.UUID) (Certificate, error) {
	var c Certificate
	err := s.pool.QueryRow(ctx, `
		SELECT `+certCols+` FROM certificates
		WHERE event_id = $1 AND ticket_id = $2 AND revoked_at IS NULL
	`, eventID, ticketID).Scan(
		&c.ID, &c.EventID, &c.TicketID, &c.OwnerID, &c.Serial, &c.RecipientName, &c.RecipientEmail,
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.RevokedAt, &c.IssuedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, ErrNotFound
	}
	return c, err
}

func (s *PostgresStore) ListByOwner(ctx context.Context, ownerID uuid.UUID) ([]Certificate, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+certCols+` FROM certificates WHERE owner_id = $1 ORDER BY issued_at`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCerts(rows)
}

func (s *PostgresStore) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Certificate, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+certCols+` FROM certificates WHERE event_id = $1 ORDER BY issued_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCerts(rows)
}

func (s *PostgresStore) Revoke(ctx context.Context, serial string) (Certificate, error) {
	var c Certificate
	err := s.pool.QueryRow(ctx, `
		UPDATE certificates SET revoked_at = COALESCE(revoked_at, now())
		WHERE serial = $1
		RETURNING `+certCols, serial,
	).Scan(
		&c.ID, &c.EventID, &c.TicketID, &c.OwnerID, &c.Serial, &c.RecipientName, &c.RecipientEmail,
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.RevokedAt, &c.IssuedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, ErrNotFound
	}
	return c, err
}

type certRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanCerts(rows certRows) ([]Certificate, error) {
	out := make([]Certificate, 0)
	for rows.Next() {
		var c Certificate
		if err := rows.Scan(
			&c.ID, &c.EventID, &c.TicketID, &c.OwnerID, &c.Serial, &c.RecipientName, &c.RecipientEmail,
			&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.RevokedAt, &c.IssuedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
