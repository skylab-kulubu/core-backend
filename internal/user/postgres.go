package user

import (
	"context"
	"errors"
	"strings"
	"time"

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

const userCols = `id, email, first_name, last_name, username, school_email, sky_number, COALESCE(student_card_uid, ''), linkedin, university, faculty, department, phone, profile_picture_id, profile_picture_url, account_state, deletion_requested_at, anonymized_at, created_at, updated_at`

func scanUser(row interface{ Scan(dest ...any) error }) (User, error) {
	var u User
	err := row.Scan(
		&u.ID, &u.Email, &u.FirstName, &u.LastName, &u.Username, &u.SchoolEmail, &u.SkyNumber, &u.StudentCardUID,
		&u.Linkedin, &u.University, &u.Faculty, &u.Department, &u.Phone, &u.ProfilePictureID, &u.ProfilePictureURL,
		&u.AccountState, &u.DeletionRequestedAt, &u.AnonymizedAt,
		&u.CreatedAt, &u.UpdatedAt,
	)
	return withStudentCardStatus(u), err
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// CanAttribute reports whether activity from a valid bearer may be linked to
// this subject. Only a current active Core identity is attributable; a deletion
// marker keeps the answer false even after the user tombstone is hard-purged.
func (s *PostgresStore) CanAttribute(ctx context.Context, id uuid.UUID) (bool, error) {
	state, err := s.AttributionState(ctx, id)
	return state == AttributionAllowed, err
}

func (s *PostgresStore) AttributionState(ctx context.Context, id uuid.UUID) (AttributionState, error) {
	var active, blocked bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND account_state = 'active'),
		       EXISTS (SELECT 1 FROM account_deletion_requests WHERE subject_id = $1)
	`, id).Scan(&active, &blocked)
	if err != nil {
		return "", err
	}
	if blocked {
		return AttributionBlocked, nil
	}
	if active {
		return AttributionAllowed, nil
	}
	return AttributionAnonymous, nil
}

func (s *PostgresStore) Upsert(ctx context.Context, u User) (User, bool, error) {
	var created bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (id, email, first_name, last_name, username, school_email, sky_number)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE NOT EXISTS (SELECT 1 FROM account_deletion_requests WHERE subject_id = $1)
		ON CONFLICT (id) DO UPDATE SET
			email = excluded.email,
			first_name = users.first_name,
			last_name = users.last_name,
			username = CASE
				WHEN excluded.username <> '' THEN excluded.username
				ELSE users.username
			END,
			school_email = CASE
				WHEN excluded.school_email <> '' THEN excluded.school_email
				ELSE users.school_email
			END,
			sky_number = CASE
				WHEN excluded.sky_number <> '' THEN excluded.sky_number
				ELSE users.sky_number
			END,
			updated_at = now()
		WHERE users.account_state = 'active'
		RETURNING `+userCols+`, (xmax = 0)
	`, u.ID, u.Email, u.FirstName, u.LastName, u.Username, u.SchoolEmail, u.SkyNumber).Scan(
		&u.ID, &u.Email, &u.FirstName, &u.LastName, &u.Username, &u.SchoolEmail, &u.SkyNumber, &u.StudentCardUID,
		&u.Linkedin, &u.University, &u.Faculty, &u.Department, &u.Phone, &u.ProfilePictureID, &u.ProfilePictureURL,
		&u.AccountState, &u.DeletionRequestedAt, &u.AnonymizedAt,
		&u.CreatedAt, &u.UpdatedAt, &created,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, ErrAccountBlocked
	}
	if isUnique(err) {
		return User{}, false, ErrConflict
	}
	if err != nil {
		return User{}, false, err
	}
	return withStudentCardStatus(u), created, nil
}

func (s *PostgresStore) UpdateProfile(ctx context.Context, u User) (User, error) {
	got, err := scanUser(s.pool.QueryRow(ctx, `
		UPDATE users SET
			first_name = $2,
			last_name = $3,
			linkedin = $4,
			university = $5,
			faculty = $6,
			department = $7,
			phone = $8,
			student_card_uid = $9,
			profile_picture_id = $10,
			profile_picture_url = $11,
			updated_at = now()
		WHERE id = $1 AND account_state = 'active'
		RETURNING `+userCols,
		u.ID, u.FirstName, u.LastName, u.Linkedin, u.University, u.Faculty, u.Department, u.Phone, u.StudentCardUID,
		u.ProfilePictureID, u.ProfilePictureURL,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		var state AccountState
		stateErr := s.pool.QueryRow(ctx, `SELECT account_state FROM users WHERE id = $1`, u.ID).Scan(&state)
		if stateErr == nil && state != AccountActive {
			return User{}, ErrAccountBlocked
		}
		return User{}, ErrNotFound
	}
	return got, err
}

func (s *PostgresStore) Search(ctx context.Context, q string) ([]User, error) {
	return s.search(ctx, q, 0)
}

func (s *PostgresStore) SearchLimit(ctx context.Context, q string, limit int) ([]User, error) {
	return s.search(ctx, q, limit)
}

func (s *PostgresStore) search(ctx context.Context, q string, limit int) ([]User, error) {
	needle := strings.TrimSpace(q)
	rows, err := s.pool.Query(ctx, `
		SELECT `+userCols+`
		FROM users
		WHERE account_state = 'active' AND (
		   lower(email) LIKE '%' || lower($1) || '%'
		   OR lower(school_email) LIKE '%' || lower($1) || '%'
		   OR lower(sky_number) LIKE '%' || lower($1) || '%'
		   OR lower(username) LIKE '%' || lower($1) || '%'
		   OR lower(first_name) LIKE '%' || lower($1) || '%'
		   OR lower(last_name) LIKE '%' || lower($1) || '%'
		   OR lower(first_name || ' ' || last_name) LIKE '%' || lower($1) || '%')
		ORDER BY last_name, first_name, email
		LIMIT CASE WHEN $2 > 0 THEN $2 ELSE NULL END
	`, needle, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *PostgresStore) FindByEmail(ctx context.Context, email string) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+userCols+`
		FROM users
		WHERE account_state = 'active' AND lower(email) = lower($1)
		ORDER BY created_at
	`, strings.TrimSpace(email))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *PostgresStore) FindByStudentCardUID(ctx context.Context, uid string) (User, error) {
	if uid == "" {
		return User{}, ErrNotFound
	}
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE account_state = 'active' AND student_card_uid = $1`, uid))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func (s *PostgresStore) SetStudentCardUID(ctx context.Context, id uuid.UUID, uid string) (User, error) {
	var uidArg any
	if uid == "" {
		uidArg = nil
	} else {
		uidArg = uid
	}
	got, err := scanUser(s.pool.QueryRow(ctx, `
		UPDATE users SET student_card_uid = $2, updated_at = now()
		WHERE id = $1 AND account_state = 'active'
		RETURNING `+userCols, id, uidArg))
	if isUnique(err) {
		return User{}, ErrConflict
	}
	if errors.Is(err, pgx.ErrNoRows) {
		var state AccountState
		stateErr := s.pool.QueryRow(ctx, `SELECT account_state FROM users WHERE id = $1`, id).Scan(&state)
		if stateErr == nil && state != AccountActive {
			return User{}, ErrAccountBlocked
		}
		return User{}, ErrNotFound
	}
	return got, err
}

func (s *PostgresStore) NextSkyNumber(ctx context.Context) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(881991)`); err != nil {
		return "", err
	}
	var max int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(CAST(SUBSTRING(sky_number FROM 5) AS INTEGER)), 0)
		FROM users
		WHERE sky_number ~ '^SKY-[0-9]+$'
	`).Scan(&max); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return FormatSkyNumber(max + 1)
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func (s *PostgresStore) RequestDeletion(ctx context.Context, id uuid.UUID, requestedBy *uuid.UUID) (DeletionRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeletionRequest{}, err
	}
	defer tx.Rollback(ctx)
	lockIDs := []uuid.UUID{id}
	if requestedBy != nil {
		lockIDs = append(lockIDs, *requestedBy)
	}
	if err := subjectlock.LockMany(ctx, tx, lockIDs...); err != nil {
		return DeletionRequest{}, err
	}
	if existing, err := scanDeletionRequest(tx.QueryRow(ctx, `SELECT `+deletionRequestCols+` FROM account_deletion_requests WHERE subject_id = $1`, id)); err == nil {
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return DeletionRequest{}, err
	}

	var state AccountState
	if err := tx.QueryRow(ctx, `SELECT account_state FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&state); errors.Is(err, pgx.ErrNoRows) {
		return DeletionRequest{}, ErrNotFound
	} else if err != nil {
		return DeletionRequest{}, err
	}
	if state != AccountActive {
		return DeletionRequest{}, ErrAccountBlocked
	}

	requestID := uuid.New()
	var request DeletionRequest
	err = tx.QueryRow(ctx, `
		INSERT INTO account_deletion_requests (id, subject_id, requested_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (subject_id) DO UPDATE SET subject_id = excluded.subject_id
		RETURNING `+deletionRequestCols+`
	`, requestID, id, requestedBy).Scan(
		&request.ID, &request.SubjectID, &request.RequestedBy, &request.Status, &request.AttemptCount,
		&request.NextAttemptAt, &request.LeaseUntil, &request.LeaseToken, &request.ProfileMediaID,
		&request.LastErrorCode, &request.CreatedAt, &request.UpdatedAt, &request.CompletedAt, &request.PlatformBlockedAt,
	)
	if subjectlock.IsInactiveAccountReference(err) {
		return DeletionRequest{}, ErrAccountBlocked
	}
	if err != nil {
		return DeletionRequest{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_deletion_outbox (id, request_id, subject_id, available_at)
		VALUES ($1, $2, $3, 'infinity'::timestamptz)
		ON CONFLICT (request_id) DO NOTHING
	`, uuid.New(), request.ID, request.SubjectID); err != nil {
		return DeletionRequest{}, err
	}
	// Insert the marker while a self-requester is still active so the actor
	// trigger can validate and preserve its audit attribution. The marker and
	// state transition remain invisible until this transaction commits.
	stateTag, err := tx.Exec(ctx, `
		UPDATE users
		SET account_state = 'deletion_pending',
			deletion_requested_at = COALESCE(deletion_requested_at, now()),
			updated_at = now()
		WHERE id = $1 AND account_state = 'active'
	`, id)
	if err != nil {
		return DeletionRequest{}, err
	}
	if stateTag.RowsAffected() != 1 {
		return DeletionRequest{}, ErrAccountBlocked
	}
	if err := tx.Commit(ctx); err != nil {
		return DeletionRequest{}, err
	}
	return request, nil
}

func (s *PostgresStore) AnonymizeAccount(ctx context.Context, id uuid.UUID, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Take the strongest required table locks in one deterministic order before
	// any row lock. SHARE ROW EXCLUSIVE blocks profile/reference writers while
	// being self-exclusive, so concurrent anonymizers serialize instead of each
	// taking SHARE and deadlocking while upgrading for the users UPDATE.
	if _, err := tx.Exec(ctx, `
		LOCK TABLE events, event_images, users, certificate_templates, certificate_template_versions
		IN SHARE ROW EXCLUSIVE MODE
	`); err != nil {
		return err
	}

	var state AccountState
	var profileMediaID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT account_state, profile_picture_id FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&state, &profileMediaID); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if state == AccountAnonymized {
		return tx.Commit(ctx)
	}
	if state != AccountDeletionPending {
		return ErrInvalid
	}
	if _, err := tx.Exec(ctx, `
		UPDATE competitors
		SET user_id = NULL, withdrawn_at = COALESCE(withdrawn_at, $2), withdrawn_by = NULL, updated_at = $2
		WHERE user_id = $1
	`, id, at); err != nil {
		return err
	}
	statements := []string{
		`UPDATE media SET uploaded_by = NULL WHERE uploaded_by = $1`,
		`UPDATE tickets SET owner_id = NULL WHERE owner_id = $1`,
		`UPDATE certificates SET owner_id = NULL, recipient_email = '' WHERE owner_id = $1`,
		`DELETE FROM event_door_staff WHERE user_id = $1`,
		`UPDATE urls SET created_by = NULL WHERE created_by = $1`,
		`UPDATE url_hits SET user_id = NULL, ip = '', user_agent = '', referer = '' WHERE user_id = $1`,
		`UPDATE events SET archived_by = NULL WHERE archived_by = $1`,
		`UPDATE event_days SET archived_by = NULL WHERE archived_by = $1`,
		`UPDATE sessions SET archived_by = NULL WHERE archived_by = $1`,
		`UPDATE seasons SET archived_by = NULL WHERE archived_by = $1`,
		`UPDATE competitors SET withdrawn_by = NULL WHERE withdrawn_by = $1`,
		`UPDATE media SET deleted_by = NULL WHERE deleted_by = $1`,
		`UPDATE urls SET disabled_by = NULL WHERE disabled_by = $1`,
		`UPDATE certificate_templates SET created_by = NULL WHERE created_by = $1`,
		`UPDATE certificate_template_versions SET published_by = NULL WHERE published_by = $1`,
		`UPDATE certificate_template_bindings SET updated_by = NULL WHERE updated_by = $1`,
		`UPDATE certificate_event_state SET attendance_finalized_by = NULL WHERE attendance_finalized_by = $1`,
		`UPDATE certificate_batches SET requested_by = NULL WHERE requested_by = $1`,
		`UPDATE account_deletion_requests SET requested_by = NULL WHERE requested_by = $1`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET
			email = '', first_name = '', last_name = '', username = '', school_email = '', sky_number = '',
			student_card_uid = NULL, linkedin = '', university = '', faculty = '', department = '', phone = '',
			profile_picture_id = NULL, profile_picture_url = '', account_state = 'anonymized',
			anonymized_at = COALESCE(anonymized_at, $2), updated_at = $2
		WHERE id = $1 AND account_state = 'deletion_pending'
	`, id, at); err != nil {
		return err
	}
	if profileMediaID != nil {
		var shared bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM events WHERE cover_image_id = $1
			UNION ALL SELECT 1 FROM event_images WHERE media_id = $1
			UNION ALL SELECT 1 FROM users WHERE profile_picture_id = $1
			UNION ALL SELECT 1 FROM certificate_templates
				WHERE draft_layout->>'backgroundMediaId' = $1::text
				   OR EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(draft_layout->'elements', '[]'::jsonb)) element WHERE element->>'mediaId' = $1::text)
			UNION ALL SELECT 1 FROM certificate_template_versions
				WHERE layout->>'backgroundMediaId' = $1::text
				   OR asset_manifest ? $1::text
				   OR EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(layout->'elements', '[]'::jsonb)) element WHERE element->>'mediaId' = $1::text)
		)`, *profileMediaID).Scan(&shared); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE account_deletion_requests
			SET profile_media_id = CASE WHEN $3 THEN NULL ELSE COALESCE(profile_media_id, $2) END,
				updated_at = $4
			WHERE subject_id = $1
		`, id, profileMediaID, shared, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE media
			SET file_name = '', uploaded_by = NULL, deleted_by = NULL,
				deleted_at = CASE WHEN $3 THEN deleted_at ELSE COALESCE(deleted_at, $2) END,
				updated_at = $2
			WHERE id = $1
		`, *profileMediaID, at, shared); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `
		UPDATE account_deletion_requests SET profile_media_id = NULL, updated_at = $2 WHERE subject_id = $1
	`, id, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// HardPurgeAccount is intentionally not exposed through Store or an HTTP service.
// It exists only for controlled retention maintenance after anonymization.
func (s *PostgresStore) HardPurgeAccount(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM users
		WHERE id = $1 AND account_state = 'anonymized'
		  AND EXISTS (
			SELECT 1 FROM account_deletion_requests
			WHERE subject_id = $1 AND status = 'completed'
		  )
	`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
