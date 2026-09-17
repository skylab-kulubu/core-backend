package event

import (
	"context"
	"errors"
	"strings"

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

const eventCols = `e.id, e.name, e.description, e.location, e.owner_team, e.form_url, e.capacity, e.start_date, e.end_date, e.linkedin, e.active, e.ranked, e.prize_info, e.season_id, e.cover_image_id, m.file_url, e.attendance_rule, e.attendance_ratio, e.created_at, e.updated_at`

const eventFrom = `events e LEFT JOIN media m ON m.id = e.cover_image_id`

func (s *PostgresStore) List(ctx context.Context, ownerTeam string, activeOnly bool) ([]Event, error) {
	q := `SELECT ` + eventCols + ` FROM ` + eventFrom
	args := []any{}
	where := make([]string, 0, 2)
	if ownerTeam != "" {
		where = append(where, `e.owner_team = $1`)
		args = append(args, ownerTeam)
	}
	if activeOnly {
		where = append(where, `e.active = true`)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY e.start_date NULLS LAST, e.created_at`
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
		if err := s.loadImages(ctx, &e); err != nil {
			return nil, err
		}
		out = append(out, emptyGallery(e))
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Event, error) {
	e, err := scanEvent(s.pool.QueryRow(ctx, `SELECT `+eventCols+` FROM `+eventFrom+` WHERE e.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	if err := s.loadImages(ctx, &e); err != nil {
		return Event{}, err
	}
	return emptyGallery(e), nil
}

func (s *PostgresStore) Create(ctx context.Context, e Event) (Event, error) {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO events (
			id, name, description, location, owner_team, form_url, capacity,
			start_date, end_date, linkedin, active, ranked, prize_info, season_id, cover_image_id,
			attendance_rule, attendance_ratio
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.FormURL, e.Capacity,
		e.StartDate, e.EndDate, e.Linkedin, e.Active, e.Ranked, e.PrizeInfo, e.SeasonID, e.CoverImageID,
		attendanceRule(e.AttendanceRule), e.AttendanceRatio)
	if err != nil {
		return Event{}, err
	}
	return s.Get(ctx, e.ID)
}

func (s *PostgresStore) Update(ctx context.Context, e Event) (Event, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE events SET
			name = $2, description = $3, location = $4, owner_team = $5, form_url = $6,
			capacity = $7, start_date = $8, end_date = $9, linkedin = $10, active = $11,
			ranked = $12, prize_info = $13, season_id = $14, cover_image_id = $15,
			attendance_rule = $16, attendance_ratio = $17, updated_at = now()
		WHERE id = $1`,
		e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.FormURL, e.Capacity,
		e.StartDate, e.EndDate, e.Linkedin, e.Active, e.Ranked, e.PrizeInfo, e.SeasonID, e.CoverImageID,
		attendanceRule(e.AttendanceRule), e.AttendanceRatio)
	if err != nil {
		return Event{}, err
	}
	if tag.RowsAffected() == 0 {
		return Event{}, ErrNotFound
	}
	return s.Get(ctx, e.ID)
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

func (s *PostgresStore) AddImages(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error) {
	if _, err := s.Get(ctx, eventID); err != nil {
		return Event{}, err
	}
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO event_images (event_id, media_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING
		`, eventID, id); err != nil {
			return Event{}, err
		}
	}
	return s.Get(ctx, eventID)
}

func (s *PostgresStore) RemoveImages(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error) {
	if _, err := s.Get(ctx, eventID); err != nil {
		return Event{}, err
	}
	for _, id := range ids {
		tag, err := s.pool.Exec(ctx, `DELETE FROM event_images WHERE event_id = $1 AND media_id = $2`, eventID, id)
		if err != nil {
			return Event{}, err
		}
		if tag.RowsAffected() == 0 {
			return Event{}, ErrNotFound
		}
	}
	return s.Get(ctx, eventID)
}

func (s *PostgresStore) loadImages(ctx context.Context, e *Event) error {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.file_url
		FROM event_images ei
		JOIN media m ON m.id = ei.media_id
		WHERE ei.event_id = $1
		ORDER BY m.created_at
	`, e.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	images := make([]GalleryImage, 0)
	urls := make([]string, 0)
	for rows.Next() {
		var im GalleryImage
		if err := rows.Scan(&im.ID, &im.URL); err != nil {
			return err
		}
		images = append(images, im)
		if im.URL != "" {
			urls = append(urls, im.URL)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	e.Images = images
	e.ImageURLs = urls
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
	rows, err := s.pool.Query(ctx, `SELECT `+eventCols+` FROM `+eventFrom+` WHERE e.season_id = $1 ORDER BY e.start_date NULLS LAST, e.created_at`, seasonID)
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
		if err := s.loadImages(ctx, &out[len(out)-1]); err != nil {
			return nil, err
		}
		out[len(out)-1] = emptyGallery(out[len(out)-1])
	}
	return out, rows.Err()
}

func (s *PostgresStore) SetSeason(ctx context.Context, eventID uuid.UUID, seasonID *uuid.UUID) (Event, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE events SET season_id = $2, updated_at = now() WHERE id = $1`, eventID, seasonID)
	if err != nil {
		return Event{}, err
	}
	if tag.RowsAffected() == 0 {
		return Event{}, ErrNotFound
	}
	return s.Get(ctx, eventID)
}

const sessionCols = `id, event_day_id, title, speaker_name, speaker_linkedin, description, start_time, end_time, order_index, session_type, cancelled`

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
			start_time, end_time, order_index, session_type, cancelled
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING `+sessionCols, sess.ID, sess.EventDayID, sess.Title, sess.SpeakerName, sess.SpeakerLinkedin, sess.Description,
		sess.StartTime, sess.EndTime, sess.OrderIndex, sess.SessionType, sess.Cancelled))
}

func (s *PostgresStore) UpdateSession(ctx context.Context, sess Session) (Session, error) {
	got, err := scanSession(s.pool.QueryRow(ctx, `
		UPDATE sessions SET
			title = $2, speaker_name = $3, speaker_linkedin = $4, description = $5,
			start_time = $6, end_time = $7, order_index = $8, session_type = $9, cancelled = $10
		WHERE id = $1
		RETURNING `+sessionCols, sess.ID, sess.Title, sess.SpeakerName, sess.SpeakerLinkedin, sess.Description,
		sess.StartTime, sess.EndTime, sess.OrderIndex, sess.SessionType, sess.Cancelled))
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
	var coverURL *string
	err := row.Scan(
		&e.ID, &e.Name, &e.Description, &e.Location, &e.OwnerTeam, &e.FormURL, &e.Capacity,
		&e.StartDate, &e.EndDate, &e.Linkedin, &e.Active, &e.Ranked, &e.PrizeInfo, &e.SeasonID,
		&e.CoverImageID, &coverURL, &e.AttendanceRule, &e.AttendanceRatio, &e.CreatedAt, &e.UpdatedAt,
	)
	if coverURL != nil {
		e.CoverImageURL = *coverURL
	}
	return e, err
}

func scanSession(row rowScanner) (Session, error) {
	var sess Session
	err := row.Scan(
		&sess.ID, &sess.EventDayID, &sess.Title, &sess.SpeakerName, &sess.SpeakerLinkedin, &sess.Description,
		&sess.StartTime, &sess.EndTime, &sess.OrderIndex, &sess.SessionType, &sess.Cancelled,
	)
	return sess, err
}

func attendanceRule(rule string) string {
	if strings.TrimSpace(rule) == "" {
		return "none"
	}
	return rule
}
