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

const eventCols = `id, name, description, location, owner_team, form_url, capacity, start_date, end_date, linkedin, active, ranked, prize_info, season_id, created_at, updated_at`

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
			start_date, end_date, linkedin, active, ranked, prize_info, season_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING `+eventCols, e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.FormURL, e.Capacity,
		e.StartDate, e.EndDate, e.Linkedin, e.Active, e.Ranked, e.PrizeInfo, e.SeasonID))
}

func (s *PostgresStore) Update(ctx context.Context, e Event) (Event, error) {
	ev, err := scanEvent(s.pool.QueryRow(ctx, `
		UPDATE events SET
			name = $2, description = $3, location = $4, owner_team = $5, form_url = $6,
			capacity = $7, start_date = $8, end_date = $9, linkedin = $10, active = $11,
			ranked = $12, prize_info = $13, season_id = $14, updated_at = now()
		WHERE id = $1
		RETURNING `+eventCols, e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.FormURL, e.Capacity,
		e.StartDate, e.EndDate, e.Linkedin, e.Active, e.Ranked, e.PrizeInfo, e.SeasonID))
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

func (s *PostgresStore) GetDay(ctx context.Context, id uuid.UUID) (Day, error) {
	var d Day
	err := s.pool.QueryRow(ctx, `
		SELECT id, event_id, name, start_date, end_date FROM event_days WHERE id = $1
	`, id).Scan(&d.ID, &d.EventID, &d.Name, &d.StartDate, &d.EndDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return Day{}, ErrNotFound
	}
	return d, err
}

func (s *PostgresStore) CreateDay(ctx context.Context, d Day) (Day, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO event_days (id, event_id, name, start_date, end_date)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, event_id, name, start_date, end_date
	`, d.ID, d.EventID, d.Name, d.StartDate, d.EndDate).Scan(&d.ID, &d.EventID, &d.Name, &d.StartDate, &d.EndDate)
	return d, err
}

func (s *PostgresStore) ListDays(ctx context.Context, eventID uuid.UUID) ([]Day, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, event_id, name, start_date, end_date FROM event_days WHERE event_id = $1 ORDER BY start_date NULLS LAST
	`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Day, 0)
	for rows.Next() {
		var d Day
		if err := rows.Scan(&d.ID, &d.EventID, &d.Name, &d.StartDate, &d.EndDate); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateDay(ctx context.Context, d Day) (Day, error) {
	err := s.pool.QueryRow(ctx, `
		UPDATE event_days SET name = $2, start_date = $3, end_date = $4, updated_at = now()
		WHERE id = $1
		RETURNING id, event_id, name, start_date, end_date
	`, d.ID, d.Name, d.StartDate, d.EndDate).Scan(&d.ID, &d.EventID, &d.Name, &d.StartDate, &d.EndDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return Day{}, ErrNotFound
	}
	return d, err
}

func (s *PostgresStore) DeleteDay(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM event_days WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListBySeason(ctx context.Context, seasonID uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+eventCols+` FROM events WHERE season_id = $1 ORDER BY start_date NULLS LAST, created_at`, seasonID)
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

func (s *PostgresStore) SetSeason(ctx context.Context, eventID uuid.UUID, seasonID *uuid.UUID) (Event, error) {
	e, err := scanEvent(s.pool.QueryRow(ctx, `
		UPDATE events SET season_id = $2, updated_at = now() WHERE id = $1 RETURNING `+eventCols, eventID, seasonID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return e, err
}

const sessionCols = `id, event_day_id, title, speaker_name, speaker_linkedin, description, start_time, end_time, order_index, session_type`

func (s *PostgresStore) GetSession(ctx context.Context, id uuid.UUID) (Session, error) {
	sess, err := scanSession(s.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return sess, err
}

func (s *PostgresStore) ListSessions(ctx context.Context, eventDayID uuid.UUID) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sessionCols+` FROM sessions WHERE event_day_id = $1 ORDER BY order_index, start_time NULLS LAST`, eventDayID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Session, 0)
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateSession(ctx context.Context, sess Session) (Session, error) {
	if sess.ID == uuid.Nil {
		sess.ID = uuid.New()
	}
	return scanSession(s.pool.QueryRow(ctx, `
		INSERT INTO sessions (
			id, event_day_id, title, speaker_name, speaker_linkedin, description,
			start_time, end_time, order_index, session_type
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING `+sessionCols, sess.ID, sess.EventDayID, sess.Title, sess.SpeakerName, sess.SpeakerLinkedin, sess.Description,
		sess.StartTime, sess.EndTime, sess.OrderIndex, sess.SessionType))
}

func (s *PostgresStore) UpdateSession(ctx context.Context, sess Session) (Session, error) {
	got, err := scanSession(s.pool.QueryRow(ctx, `
		UPDATE sessions SET
			title = $2, speaker_name = $3, speaker_linkedin = $4, description = $5,
			start_time = $6, end_time = $7, order_index = $8, session_type = $9
		WHERE id = $1
		RETURNING `+sessionCols, sess.ID, sess.Title, sess.SpeakerName, sess.SpeakerLinkedin, sess.Description,
		sess.StartTime, sess.EndTime, sess.OrderIndex, sess.SessionType))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return got, err
}

func (s *PostgresStore) DeleteSession(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
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
		&e.StartDate, &e.EndDate, &e.Linkedin, &e.Active, &e.Ranked, &e.PrizeInfo, &e.SeasonID, &e.CreatedAt, &e.UpdatedAt,
	)
	return e, err
}

func scanSession(row rowScanner) (Session, error) {
	var sess Session
	err := row.Scan(
		&sess.ID, &sess.EventDayID, &sess.Title, &sess.SpeakerName, &sess.SpeakerLinkedin, &sess.Description,
		&sess.StartTime, &sess.EndTime, &sess.OrderIndex, &sess.SessionType,
	)
	return sess, err
}
