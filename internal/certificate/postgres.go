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

const certCols = `id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, template_version_id, template_source, pdf_key, pdf_sha256, batch_id, job_id, revoked_at, issued_at, attended_count, counted_total, program_minutes, co_organizer_teams`

// certScan reads certCols into a Certificate: the attendance snapshot's
// three columns are NULL together for a certificate issued before core kept
// it (the table's check).
type certScan struct {
	c                        Certificate
	attended, total, minutes *int
}

func (r *certScan) dest(extra ...any) []any {
	c := &r.c
	return append([]any{
		&c.ID, &c.EventID, &c.TicketID, &c.OwnerID, &c.Serial, &c.RecipientName, &c.RecipientEmail,
		&c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.TemplateVersionID, &c.TemplateSource,
		&c.PDFKey, &c.PDFSHA256, &c.BatchID, &c.JobID, &c.RevokedAt, &c.IssuedAt,
		&r.attended, &r.total, &r.minutes, &c.CoOrganizerTeams,
	}, extra...)
}

func (r *certScan) cert() Certificate {
	c := r.c
	c.Attendance = nil
	if r.attended != nil && r.total != nil && r.minutes != nil {
		c.Attendance = &AttendanceSnapshot{Attended: *r.attended, CountedTotal: *r.total, ProgramMinutes: *r.minutes}
	}
	return c
}

// snapshotArgs are the snapshot's three column values, NULL without one.
func snapshotArgs(a *AttendanceSnapshot) (attended, total, minutes *int) {
	if a == nil {
		return nil, nil, nil
	}
	return &a.Attended, &a.CountedTotal, &a.ProgramMinutes
}

// insertCertSQL writes a certificate with its PDF and attendance snapshot.
const insertCertSQL = `
		INSERT INTO certificates (
			id, event_id, ticket_id, owner_id, serial, recipient_name, recipient_email,
			event_name, owner_team, verify_url, template_version_id, template_source,
			pdf_key, pdf_sha256, batch_id, job_id, pdf, issued_at,
			attended_count, counted_total, program_minutes, co_organizer_teams
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,now(),$18,$19,$20,$21)
		RETURNING ` + certCols

func insertCertArgs(c Certificate, pdf []byte) []any {
	attended, total, minutes := snapshotArgs(c.Attendance)
	return []any{
		c.ID, c.EventID, c.TicketID, c.OwnerID, c.Serial, c.RecipientName, c.RecipientEmail,
		c.EventName, c.OwnerTeam, c.VerifyURL, c.TemplateVersionID, c.TemplateSource,
		c.PDFKey, c.PDFSHA256, c.BatchID, c.JobID, pdf, attended, total, minutes, c.CoOrganizerTeams,
	}
}

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
	var row certScan
	err := s.pool.QueryRow(ctx, insertCertSQL, insertCertArgs(c, pdf)...).Scan(row.dest()...)
	if isUnique(err) {
		return Certificate{}, ErrConflict
	}
	if subjectlock.IsInactiveAccountReference(err) {
		return Certificate{}, ErrInvalid
	}
	if err != nil {
		return Certificate{}, err
	}
	return row.cert(), nil
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
	var row certScan
	err = tx.QueryRow(ctx, insertCertSQL, insertCertArgs(c, pdf)...).Scan(row.dest()...)
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
	return row.cert(), nil
}

func (s *PostgresStore) GetBySerial(ctx context.Context, serial string) (Certificate, []byte, error) {
	var row certScan
	var pdf []byte
	err := s.pool.QueryRow(ctx, `SELECT `+certCols+`, pdf FROM certificates WHERE serial = $1`, serial).Scan(row.dest(&pdf)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, nil, ErrNotFound
	}
	if err != nil {
		return Certificate{}, nil, err
	}
	return row.cert(), pdf, nil
}

func (s *PostgresStore) GetActive(ctx context.Context, eventID, ticketID uuid.UUID) (Certificate, error) {
	var row certScan
	err := s.pool.QueryRow(ctx, `
		SELECT `+certCols+` FROM certificates
		WHERE event_id = $1 AND ticket_id = $2 AND revoked_at IS NULL
	`, eventID, ticketID).Scan(row.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, ErrNotFound
	}
	if err != nil {
		return Certificate{}, err
	}
	return row.cert(), nil
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
	var row certScan
	err := s.pool.QueryRow(ctx, `
		UPDATE certificates SET revoked_at = COALESCE(revoked_at, now())
		WHERE serial = $1
		RETURNING `+certCols, serial,
	).Scan(row.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, ErrNotFound
	}
	if err != nil {
		return Certificate{}, err
	}
	return row.cert(), nil
}

type certRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanCerts(rows certRows) ([]Certificate, error) {
	out := make([]Certificate, 0)
	for rows.Next() {
		var row certScan
		if err := rows.Scan(row.dest()...); err != nil {
			return nil, err
		}
		out = append(out, row.cert())
	}
	return out, rows.Err()
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
