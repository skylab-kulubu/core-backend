package certificate

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const certCols = `id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, template_version_id, template_source, pdf_key, pdf_sha256, batch_id, job_id, revoked_at, issued_at`

func (s *PostgresStore) Create(ctx context.Context, c Certificate, pdf []byte) (Certificate, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.TemplateSource == "" {
		c.TemplateSource = "legacy"
	}
	if pdf == nil {
		pdf = []byte{}
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO certificates (
			id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email,
			event_name, owner_team, verify_url, template_version_id, template_source,
			pdf_key, pdf_sha256, batch_id, job_id, pdf, issued_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,now())
		RETURNING `+certCols,
		c.ID, c.EventID, c.TicketID, c.OwnerID, c.Serial, c.RecipientName, c.RecipientEmail,
		c.EventName, c.OwnerTeam, c.VerifyURL, c.TemplateVersionID, c.TemplateSource,
		c.PDFKey, c.PDFSHA256, c.BatchID, c.JobID, pdf,
	).Scan(
		&c.ID, &c.EventID, &c.TicketID, &c.OwnerID, &c.Serial, &c.RecipientName, &c.RecipientEmail,
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.TemplateVersionID, &c.TemplateSource,
		&c.PDFKey, &c.PDFSHA256, &c.BatchID, &c.JobID, &c.RevokedAt, &c.IssuedAt,
	)
	if isUnique(err) {
		return Certificate{}, ErrConflict
	}
	if subjectlock.IsInactiveAccountReference(err) {
		return Certificate{}, ErrInvalid
	}
	return c, err
}

func (s *PostgresStore) Replace(ctx context.Context, previousSerial string, c Certificate, pdf []byte) (Certificate, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.TemplateSource == "" {
		c.TemplateSource = "legacy"
	}
	if pdf == nil {
		pdf = []byte{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Certificate{}, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE certificates SET revoked_at=now()
		WHERE serial=$1 AND event_id=$2 AND ticket_id=$3 AND revoked_at IS NULL`,
		previousSerial, c.EventID, c.TicketID)
	if err != nil {
		return Certificate{}, err
	}
	if tag.RowsAffected() != 1 {
		return Certificate{}, ErrConflict
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO certificates (
			id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email,
			event_name, owner_team, verify_url, template_version_id, template_source,
			pdf_key, pdf_sha256, batch_id, job_id, pdf, issued_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,now())
		RETURNING `+certCols,
		c.ID, c.EventID, c.TicketID, c.OwnerID, c.Serial, c.RecipientName, c.RecipientEmail,
		c.EventName, c.OwnerTeam, c.VerifyURL, c.TemplateVersionID, c.TemplateSource,
		c.PDFKey, c.PDFSHA256, c.BatchID, c.JobID, pdf,
	).Scan(
		&c.ID, &c.EventID, &c.TicketID, &c.OwnerID, &c.Serial, &c.RecipientName, &c.RecipientEmail,
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.TemplateVersionID, &c.TemplateSource,
		&c.PDFKey, &c.PDFSHA256, &c.BatchID, &c.JobID, &c.RevokedAt, &c.IssuedAt,
	)
	if isUnique(err) {
		return Certificate{}, ErrConflict
	}
	if subjectlock.IsInactiveAccountReference(err) {
		return Certificate{}, ErrInvalid
	}
	if err != nil {
		return Certificate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Certificate{}, err
	}
	return c, nil
}

func (s *PostgresStore) GetBySerial(ctx context.Context, serial string) (Certificate, []byte, error) {
	var c Certificate
	var pdf []byte
	err := s.pool.QueryRow(ctx, `SELECT `+certCols+`, pdf FROM certificates WHERE serial = $1`, serial).Scan(
		&c.ID, &c.EventID, &c.TicketID, &c.OwnerID, &c.Serial, &c.RecipientName, &c.RecipientEmail,
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.TemplateVersionID, &c.TemplateSource,
		&c.PDFKey, &c.PDFSHA256, &c.BatchID, &c.JobID, &c.RevokedAt, &c.IssuedAt, &pdf,
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
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.TemplateVersionID, &c.TemplateSource,
		&c.PDFKey, &c.PDFSHA256, &c.BatchID, &c.JobID, &c.RevokedAt, &c.IssuedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, ErrNotFound
	}
	return c, err
}

func (s *PostgresStore) ListByOwner(ctx context.Context, ownerID uuid.UUID) ([]Certificate, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+certCols+` FROM certificates WHERE owner_id = $1 ORDER BY issued_at DESC`, ownerID)
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
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.TemplateVersionID, &c.TemplateSource,
		&c.PDFKey, &c.PDFSHA256, &c.BatchID, &c.JobID, &c.RevokedAt, &c.IssuedAt,
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
			&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.TemplateVersionID, &c.TemplateSource,
			&c.PDFKey, &c.PDFSHA256, &c.BatchID, &c.JobID, &c.RevokedAt, &c.IssuedAt,
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
