package event

import (
	"context"
	"errors"

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

const eventCols = `id, name, description, location, owner_team, form_url, capacity, start_date, end_date, linkedin, active, ranked, prize_info, created_at, updated_at`

func (s *PostgresStore) List(ctx context.Context, ownerTeam string) ([]Event, error) {
	q := `SELECT ` + eventCols + ` FROM events`
	args := []any{}
	if ownerTeam != "" {
		q += ` WHERE owner_team = $1`
		args = append(args, ownerTeam)
	}
	q += ` ORDER BY start_date NULLS LAST, created_at`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Event, 0)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Event, error) {
	e, err := scanEvent(s.pool.QueryRow(ctx, `SELECT `+eventCols+` FROM events WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return e, err
}

func (s *PostgresStore) Create(ctx context.Context, e Event) (Event, error) {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	return scanEvent(s.pool.QueryRow(ctx, `
		INSERT INTO events (
			id, name, description, location, owner_team, form_url, capacity,
			start_date, end_date, linkedin, active, ranked, prize_info
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+eventCols, e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.FormURL, e.Capacity,
		e.StartDate, e.EndDate, e.Linkedin, e.Active, e.Ranked, e.PrizeInfo))
}

func (s *PostgresStore) Update(ctx context.Context, e Event) (Event, error) {
	ev, err := scanEvent(s.pool.QueryRow(ctx, `
		UPDATE events SET
			name = $2, description = $3, location = $4, owner_team = $5, form_url = $6,
			capacity = $7, start_date = $8, end_date = $9, linkedin = $10, active = $11,
			ranked = $12, prize_info = $13, updated_at = now()
		WHERE id = $1
		RETURNING `+eventCols, e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.FormURL, e.Capacity,
		e.StartDate, e.EndDate, e.Linkedin, e.Active, e.Ranked, e.PrizeInfo))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return ev, err
}

func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(row rowScanner) (Event, error) {
	var e Event
	err := row.Scan(
		&e.ID, &e.Name, &e.Description, &e.Location, &e.OwnerTeam, &e.FormURL, &e.Capacity,
		&e.StartDate, &e.EndDate, &e.Linkedin, &e.Active, &e.Ranked, &e.PrizeInfo, &e.CreatedAt, &e.UpdatedAt,
	)
	return e, err
}
