package event

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const eventCols = `e.id, e.name, e.description, e.location, e.owner_team, e.form_url, e.capacity, e.start_date, e.end_date, e.linkedin, e.active, e.ranked, e.prize_info, e.season_id, e.cover_image_id, m.file_url, COALESCE(m.cover_colors, '{}'), e.attendance_rule, e.attendance_ratio, e.extra_form_urls, e.mail_list_id, e.archived_at, e.archived_by, e.created_at, e.updated_at`

const eventFrom = `events e LEFT JOIN media m ON m.id = e.cover_image_id AND m.deleted_at IS NULL`

func (s *PostgresStore) List(ctx context.Context, ownerTeam string, activeOnly bool) ([]Event, error) {
	return s.list(ctx, ownerTeam, activeOnly, lifecycle.CurrentOnly)
}

func (s *PostgresStore) ListLifecycle(ctx context.Context, ownerTeam string, visibility lifecycle.Visibility) ([]Event, error) {
	return s.list(ctx, ownerTeam, false, visibility)
}

func (s *PostgresStore) list(ctx context.Context, ownerTeam string, activeOnly bool, visibility lifecycle.Visibility) ([]Event, error) {
	q := `SELECT ` + eventCols + ` FROM ` + eventFrom
	args := []any{}
	where := make([]string, 0, 3)
	if condition := visibility.SQLCondition("e.archived_at"); condition != "" {
		where = append(where, condition)
	}
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
		if err := s.loadDoorStaff(ctx, &e); err != nil {
			return nil, err
		}
		out = append(out, emptyGallery(e))
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Event, error) {
	return s.get(ctx, id, false)
}

func (s *PostgresStore) GetIncludingArchived(ctx context.Context, id uuid.UUID) (Event, error) {
	return s.get(ctx, id, true)
}

func (s *PostgresStore) get(ctx context.Context, id uuid.UUID, includeArchived bool) (Event, error) {
	q := `SELECT ` + eventCols + ` FROM ` + eventFrom + ` WHERE e.id = $1`
	if !includeArchived {
		q += ` AND e.archived_at IS NULL`
	}
	e, err := scanEvent(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	if err := s.loadImages(ctx, &e); err != nil {
		return Event{}, err
	}
	if err := s.loadDoorStaff(ctx, &e); err != nil {
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
			attendance_rule, attendance_ratio, extra_form_urls
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.FormURL, e.Capacity,
		e.StartDate, e.EndDate, e.Linkedin, e.Active, e.Ranked, e.PrizeInfo, e.SeasonID, e.CoverImageID,
		attendanceRule(e.AttendanceRule), e.AttendanceRatio, extraFormBytes(e))
	if err != nil {
		return Event{}, err
	}
	if err := s.replaceDoorStaff(ctx, e.ID, e.DoorStaffIDs); err != nil {
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
			attendance_rule = $16, attendance_ratio = $17, extra_form_urls = $18, updated_at = now()
		WHERE id = $1 AND archived_at IS NULL`,
		e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.FormURL, e.Capacity,
		e.StartDate, e.EndDate, e.Linkedin, e.Active, e.Ranked, e.PrizeInfo, e.SeasonID, e.CoverImageID,
		attendanceRule(e.AttendanceRule), e.AttendanceRatio, extraFormBytes(e))
	if err != nil {
		return Event{}, err
	}
	if tag.RowsAffected() == 0 {
		return Event{}, ErrNotFound
	}
	if err := s.replaceDoorStaff(ctx, e.ID, e.DoorStaffIDs); err != nil {
		return Event{}, err
	}
	return s.Get(ctx, e.ID)
}

func (s *PostgresStore) Archive(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE events
		SET archived_at = COALESCE(archived_at, now()),
			archived_by = CASE WHEN archived_at IS NULL THEN $2 ELSE archived_by END,
			updated_at = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id, actorID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Restore(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE events
		SET archived_at = NULL,
			archived_by = NULL,
			updated_at = CASE WHEN archived_at IS NOT NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id)
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
		JOIN media m ON m.id = ei.media_id AND m.deleted_at IS NULL
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

func (s *PostgresStore) loadDoorStaff(ctx context.Context, e *Event) error {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id FROM event_door_staff WHERE event_id = $1 ORDER BY user_id
	`, e.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	e.DoorStaffIDs = ids
	return nil
}

func (s *PostgresStore) replaceDoorStaff(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM event_door_staff WHERE event_id = $1`, eventID); err != nil {
		return err
	}
	seen := map[uuid.UUID]struct{}{}
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO event_door_staff (event_id, user_id) VALUES ($1, $2)
		`, eventID, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) GetDay(ctx context.Context, id uuid.UUID) (Day, error) {
	return s.getDay(ctx, id, false)
}

func (s *PostgresStore) GetDayIncludingArchived(ctx context.Context, id uuid.UUID) (Day, error) {
	return s.getDay(ctx, id, true)
}

const dayCols = `id, event_id, name, start_date, end_date, archived_at, archived_by`

func (s *PostgresStore) getDay(ctx context.Context, id uuid.UUID, includeArchived bool) (Day, error) {
	var d Day
	q := `SELECT ` + dayCols + ` FROM event_days WHERE id = $1`
	if !includeArchived {
		q += ` AND archived_at IS NULL
			AND EXISTS (
				SELECT 1 FROM events
				WHERE events.id = event_days.event_id AND events.archived_at IS NULL
			)`
	}
	err := s.pool.QueryRow(ctx, q, id).Scan(&d.ID, &d.EventID, &d.Name, &d.StartDate, &d.EndDate, &d.ArchivedAt, &d.ArchivedBy)
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
		RETURNING `+dayCols+`
	`, d.ID, d.EventID, d.Name, d.StartDate, d.EndDate).Scan(&d.ID, &d.EventID, &d.Name, &d.StartDate, &d.EndDate, &d.ArchivedAt, &d.ArchivedBy)
	return d, err
}

func (s *PostgresStore) ListDays(ctx context.Context, eventID uuid.UUID) ([]Day, error) {
	return s.listDays(ctx, eventID, lifecycle.CurrentOnly, true)
}

func (s *PostgresStore) ListDaysLifecycle(ctx context.Context, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Day, error) {
	return s.listDays(ctx, eventID, visibility, false)
}

func (s *PostgresStore) listDays(ctx context.Context, eventID uuid.UUID, visibility lifecycle.Visibility, requireCurrentParent bool) ([]Day, error) {
	q := `SELECT ` + dayCols + ` FROM event_days WHERE event_id = $1`
	if condition := visibility.SQLCondition("archived_at"); condition != "" {
		q += ` AND ` + condition
	}
	if requireCurrentParent {
		q += ` AND EXISTS (
			SELECT 1 FROM events
			WHERE events.id = event_days.event_id AND events.archived_at IS NULL
		)`
	}
	q += ` ORDER BY start_date NULLS LAST`
	rows, err := s.pool.Query(ctx, q, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Day, 0)
	for rows.Next() {
		var d Day
		if err := rows.Scan(&d.ID, &d.EventID, &d.Name, &d.StartDate, &d.EndDate, &d.ArchivedAt, &d.ArchivedBy); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateDay(ctx context.Context, d Day) (Day, error) {
	err := s.pool.QueryRow(ctx, `
		UPDATE event_days SET name = $2, start_date = $3, end_date = $4, updated_at = now()
		WHERE id = $1 AND archived_at IS NULL
		RETURNING `+dayCols+`
	`, d.ID, d.Name, d.StartDate, d.EndDate).Scan(&d.ID, &d.EventID, &d.Name, &d.StartDate, &d.EndDate, &d.ArchivedAt, &d.ArchivedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Day{}, ErrNotFound
	}
	return d, err
}

func (s *PostgresStore) ArchiveDay(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE event_days
		SET archived_at = COALESCE(archived_at, now()),
			archived_by = CASE WHEN archived_at IS NULL THEN $2 ELSE archived_by END,
			updated_at = CASE WHEN archived_at IS NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id, actorID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) RestoreDay(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE event_days
		SET archived_at = NULL,
			archived_by = NULL,
			updated_at = CASE WHEN archived_at IS NOT NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListBySeason(ctx context.Context, seasonID uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+eventCols+` FROM `+eventFrom+` WHERE e.season_id = $1 AND e.archived_at IS NULL ORDER BY e.start_date NULLS LAST, e.created_at`, seasonID)
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
		if err := s.loadDoorStaff(ctx, &out[len(out)-1]); err != nil {
			return nil, err
		}
		out[len(out)-1] = emptyGallery(out[len(out)-1])
	}
	return out, rows.Err()
}

func (s *PostgresStore) SetSeason(ctx context.Context, eventID uuid.UUID, seasonID *uuid.UUID) (Event, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE events SET season_id = $2, updated_at = now() WHERE id = $1 AND archived_at IS NULL`, eventID, seasonID)
	if err != nil {
		return Event{}, err
	}
	if tag.RowsAffected() == 0 {
		return Event{}, ErrNotFound
	}
	return s.Get(ctx, eventID)
}

func (s *PostgresStore) SetMailListID(ctx context.Context, eventID, listID uuid.UUID) (Event, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE events SET mail_list_id = $2, updated_at = now() WHERE id = $1 AND archived_at IS NULL`, eventID, listID)
	if err != nil {
		return Event{}, err
	}
	if tag.RowsAffected() == 0 {
		return Event{}, ErrNotFound
	}
	return s.Get(ctx, eventID)
}

const sessionCols = `id, event_day_id, title, speaker_name, speaker_linkedin, description, start_time, end_time, order_index, session_type, cancelled, archived_at, archived_by`

func (s *PostgresStore) GetSession(ctx context.Context, id uuid.UUID) (Session, error) {
	return s.getSession(ctx, id, false)
}

func (s *PostgresStore) GetSessionIncludingArchived(ctx context.Context, id uuid.UUID) (Session, error) {
	return s.getSession(ctx, id, true)
}

func (s *PostgresStore) getSession(ctx context.Context, id uuid.UUID, includeArchived bool) (Session, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions WHERE id = $1`
	if !includeArchived {
		q += ` AND archived_at IS NULL
			AND EXISTS (
				SELECT 1
				FROM event_days
				JOIN events ON events.id = event_days.event_id
				WHERE event_days.id = sessions.event_day_id
				  AND event_days.archived_at IS NULL
				  AND events.archived_at IS NULL
			)`
	}
	sess, err := scanSession(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return sess, err
}

func (s *PostgresStore) ListSessions(ctx context.Context, eventDayID uuid.UUID) ([]Session, error) {
	return s.listSessions(ctx, eventDayID, lifecycle.CurrentOnly, true)
}

func (s *PostgresStore) ListSessionsLifecycle(ctx context.Context, eventDayID uuid.UUID, visibility lifecycle.Visibility) ([]Session, error) {
	return s.listSessions(ctx, eventDayID, visibility, false)
}

func (s *PostgresStore) listSessions(ctx context.Context, eventDayID uuid.UUID, visibility lifecycle.Visibility, requireCurrentParents bool) ([]Session, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions WHERE event_day_id = $1`
	if condition := visibility.SQLCondition("archived_at"); condition != "" {
		q += ` AND ` + condition
	}
	if requireCurrentParents {
		q += ` AND EXISTS (
			SELECT 1
			FROM event_days
			JOIN events ON events.id = event_days.event_id
			WHERE event_days.id = sessions.event_day_id
			  AND event_days.archived_at IS NULL
			  AND events.archived_at IS NULL
		)`
	}
	q += ` ORDER BY order_index, start_time NULLS LAST`
	rows, err := s.pool.Query(ctx, q, eventDayID)
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
		WHERE id = $1 AND archived_at IS NULL
		RETURNING `+sessionCols, sess.ID, sess.Title, sess.SpeakerName, sess.SpeakerLinkedin, sess.Description,
		sess.StartTime, sess.EndTime, sess.OrderIndex, sess.SessionType, sess.Cancelled))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return got, err
}

func (s *PostgresStore) ArchiveSession(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions
		SET archived_at = COALESCE(archived_at, now()),
			archived_by = CASE WHEN archived_at IS NULL THEN $2 ELSE archived_by END
		WHERE id = $1`, id, actorID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) RestoreSession(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET archived_at = NULL, archived_by = NULL WHERE id = $1`, id)
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

func extraFormBytes(e Event) []byte {
	raw, err := EncodeExtraForm(e.FormAlias, e.ExtraFormURLs)
	if err != nil {
		return []byte(`[]`)
	}
	return raw
}

func scanEvent(row rowScanner) (Event, error) {
	var e Event
	var coverURL *string
	var extraRaw []byte
	err := row.Scan(
		&e.ID, &e.Name, &e.Description, &e.Location, &e.OwnerTeam, &e.FormURL, &e.Capacity,
		&e.StartDate, &e.EndDate, &e.Linkedin, &e.Active, &e.Ranked, &e.PrizeInfo, &e.SeasonID,
		&e.CoverImageID, &coverURL, &e.CoverColors, &e.AttendanceRule, &e.AttendanceRatio, &extraRaw, &e.MailListID, &e.ArchivedAt, &e.ArchivedBy, &e.CreatedAt, &e.UpdatedAt,
	)
	if coverURL != nil {
		e.CoverImageURL = *coverURL
	}
	if e.CoverColors == nil {
		e.CoverColors = []string{}
	}
	if err != nil {
		return e, err
	}
	alias, extra, decErr := DecodeExtraForm(extraRaw)
	if decErr != nil {
		return e, decErr
	}
	e.FormAlias = alias
	e.ExtraFormURLs = extra
	return e, nil
}

func scanSession(row rowScanner) (Session, error) {
	var sess Session
	err := row.Scan(
		&sess.ID, &sess.EventDayID, &sess.Title, &sess.SpeakerName, &sess.SpeakerLinkedin, &sess.Description,
		&sess.StartTime, &sess.EndTime, &sess.OrderIndex, &sess.SessionType, &sess.Cancelled, &sess.ArchivedAt, &sess.ArchivedBy,
	)
	return sess, err
}

func attendanceRule(rule string) string {
	if strings.TrimSpace(rule) == "" {
		return "none"
	}
	return rule
}
