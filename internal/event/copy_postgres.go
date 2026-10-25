package event

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

// CompetitionCopier copies the source Event's current Competitions and
// their stages into the copy, inside the copy's transaction, their stage
// dates moved by shift. The competition package has it
// (competition.CopyForEvent); the event package cannot import it.
type CompetitionCopier func(ctx context.Context, tx pgx.Tx, source, copy uuid.UUID, shift func(time.Time) time.Time, by *uuid.UUID) error

// CopyCompetitionsWith gives the store the Competitions' copier: without
// it, a copy asked to bring Competitions fails.
func (s *PostgresStore) CopyCompetitionsWith(copier CompetitionCopier) *PostgresStore {
	s.copyCompetitions = copier
	return s
}

// errNoCompetitionCopier is a copy asked to bring Competitions on a store
// that was given no copier: a wiring mistake, never the caller's.
var errNoCompetitionCopier = errors.New("event: copy: no competition copier")

// copySeasonSQL is the copy's Season: the current one its new start ($1)
// falls in, the narrowest when several do. A Season without an end runs on;
// one without a start covers nothing.
const copySeasonSQL = `(SELECT id FROM seasons
	WHERE archived_at IS NULL AND start_date IS NOT NULL AND start_date <= $1
		AND (end_date IS NULL OR end_date >= $1)
	ORDER BY end_date - start_date NULLS LAST, start_date DESC, id
	LIMIT 1)`

func (s *PostgresStore) Copy(ctx context.Context, plan CopyPlan) (Event, []string, error) {
	e := plan.Event
	skippedParts := map[string]bool{}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, nil, err
	}
	defer tx.Rollback(ctx)
	// The source, locked against a concurrent archive: a copy is made
	// from a current Event.
	var sourceID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM events WHERE id = $1 AND archived_at IS NULL FOR SHARE`, plan.Source).Scan(&sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, nil, ErrNotFound
	}
	if err != nil {
		return Event{}, nil, err
	}
	// A draft, never shown as active.
	tag, err := tx.Exec(ctx, `
		INSERT INTO events (
			id, name, description, location, owner_team, capacity,
			start_date, end_date, ranked, prize_info, season_id,
			attendance_rule, attendance_ratio, status, copied_from_event_id,
			registration_closes_at, feedback_mode, feedback_auto_send
		) VALUES ($2, $3, $4, $5, $6, $7, $1, $8, $9, $10, `+copySeasonSQL+`,
			$11, $12, 'draft', $13, $14, $15, $16)
		ON CONFLICT (id) DO NOTHING`,
		e.StartDate, e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.Capacity,
		e.EndDate, e.Ranked, e.PrizeInfo, attendanceRule(e.AttendanceRule), e.AttendanceRatio,
		plan.Source, e.RegistrationClosesAt, string(feedbackMode(e.FeedbackMode)), e.FeedbackAutoSend)
	if err != nil {
		return Event{}, nil, err
	}
	if tag.RowsAffected() == 0 {
		return Event{}, nil, ErrConflict
	}
	if err := copyCover(ctx, tx, plan, skippedParts); err != nil {
		return Event{}, nil, err
	}
	if err := copyDoorStaff(ctx, tx, plan, skippedParts); err != nil {
		return Event{}, nil, err
	}
	if err := copySchedule(ctx, tx, plan); err != nil {
		return Event{}, nil, err
	}
	if err := copyTemplateBinding(ctx, tx, plan, skippedParts); err != nil {
		return Event{}, nil, err
	}
	if err := s.copyCompetitionsOf(ctx, tx, plan, skippedParts); err != nil {
		return Event{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, nil, err
	}
	created, err := s.Get(ctx, e.ID)
	return created, skipped(skippedParts), err
}

// copyCover links the cover, in a savepoint: a cover the database refuses
// (the Media's state or the Team media library, decided by the trigger the
// link writes) is left out and the copy goes on.
func copyCover(ctx context.Context, tx pgx.Tx, plan CopyPlan, skippedParts map[string]bool) error {
	switch {
	case plan.Cover == CopyRefused:
		skippedParts[SkippedCover] = true
		return nil
	case plan.Cover != CopyWrite || plan.Event.CoverImageID == nil:
		return nil
	}
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	_, err = savepoint.Exec(ctx, `UPDATE events SET cover_image_id = $2 WHERE id = $1`, plan.Event.ID, plan.Event.CoverImageID)
	if _, refused := media.DatabaseLinkRefusal(err); refused {
		skippedParts[SkippedCover] = true
		return savepoint.Rollback(ctx)
	}
	if err != nil {
		return err
	}
	return savepoint.Commit(ctx)
}

// copyDoorStaff copies the source's door staff whose accounts are active.
func copyDoorStaff(ctx context.Context, tx pgx.Tx, plan CopyPlan, skippedParts map[string]bool) error {
	switch plan.DoorStaff {
	case CopyRefused:
		var has bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_door_staff WHERE event_id = $1)`, plan.Source).Scan(&has); err != nil {
			return err
		}
		skippedParts[SkippedDoorStaff] = has
		return nil
	case CopyWrite:
		_, err := tx.Exec(ctx, `
			INSERT INTO event_door_staff (event_id, user_id)
			SELECT $2, staff.user_id FROM event_door_staff staff
			JOIN users u ON u.id = staff.user_id AND u.account_state = 'active'
			WHERE staff.event_id = $1
				AND NOT EXISTS (SELECT 1 FROM account_deletion_requests r WHERE r.subject_id = staff.user_id)`,
			plan.Source, plan.Event.ID)
		if subjectlock.IsInactiveAccountReference(err) {
			return ErrForbidden
		}
		return err
	default:
		return nil
	}
}

// copySchedule copies the source's current days and their current
// Sessions, moved; a Session is not cancelled in the copy (that year's
// cancellation is not part of the structure).
func copySchedule(ctx context.Context, tx pgx.Tx, plan CopyPlan) error {
	rows, err := tx.Query(ctx, `
		SELECT id, name, start_date, end_date FROM event_days
		WHERE event_id = $1 AND archived_at IS NULL
		ORDER BY start_date NULLS LAST, created_at, id`, plan.Source)
	if err != nil {
		return err
	}
	days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Day, error) {
		var d Day
		err := row.Scan(&d.ID, &d.Name, &d.StartDate, &d.EndDate)
		return d, err
	})
	if err != nil {
		return err
	}
	for _, day := range days {
		dayID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_days (id, event_id, name, start_date, end_date) VALUES ($1, $2, $3, $4, $5)`,
			dayID, plan.Event.ID, day.Name, plan.Shift.At(day.StartDate), plan.Shift.At(day.EndDate)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT title, speaker_name, speaker_linkedin, description, start_time, end_time, order_index, session_type, feedback_auto_send
			FROM sessions WHERE event_day_id = $1 AND archived_at IS NULL
			ORDER BY order_index, start_time NULLS LAST, id`, day.ID)
		if err != nil {
			return err
		}
		sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) {
			var sess Session
			err := row.Scan(&sess.Title, &sess.SpeakerName, &sess.SpeakerLinkedin, &sess.Description,
				&sess.StartTime, &sess.EndTime, &sess.OrderIndex, &sess.SessionType, &sess.FeedbackAutoSend)
			return sess, err
		})
		if err != nil {
			return err
		}
		for _, sess := range sessions {
			if _, err := tx.Exec(ctx, `
				INSERT INTO sessions (id, event_day_id, title, speaker_name, speaker_linkedin, description,
					start_time, end_time, order_index, session_type, cancelled, feedback_auto_send)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, false, $11)`,
				uuid.New(), dayID, sess.Title, sess.SpeakerName, sess.SpeakerLinkedin, sess.Description,
				plan.Shift.At(sess.StartTime), plan.Shift.At(sess.EndTime), sess.OrderIndex, sess.SessionType, sess.FeedbackAutoSend); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyTemplateBinding binds the copy to the source's Event template, when
// the copier may bind and the template may serve the copy's Owner team (a
// current template of no team or of that team, as the binding rule says).
func copyTemplateBinding(ctx context.Context, tx pgx.Tx, plan CopyPlan, skippedParts map[string]bool) error {
	var templateID uuid.UUID
	var fits bool
	err := tx.QueryRow(ctx, `
		SELECT b.template_id,
			t.archived_at IS NULL AND upper(btrim(t.owner_team)) IN ('', upper(btrim($2)))
		FROM certificate_template_bindings b
		JOIN certificate_templates t ON t.id = b.template_id
		WHERE b.scope = 'event' AND b.scope_key = $1`, plan.Source.String(), plan.Event.OwnerTeam).Scan(&templateID, &fits)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if plan.CertificateTemplate != CopyWrite || !fits {
		skippedParts[SkippedCertificateTemplate] = true
		return nil
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO certificate_template_bindings (id, scope, scope_key, template_id, updated_by)
		VALUES ($1, 'event', $2, $3, (SELECT id FROM users WHERE id = $4))`,
		uuid.New(), plan.Event.ID.String(), templateID, plan.By)
	if subjectlock.IsInactiveAccountReference(err) {
		return ErrForbidden
	}
	return err
}

func (s *PostgresStore) copyCompetitionsOf(ctx context.Context, tx pgx.Tx, plan CopyPlan, skippedParts map[string]bool) error {
	switch plan.Competitions {
	case CopyRefused:
		var has bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM competitions WHERE event_id = $1 AND archived_at IS NULL)`, plan.Source).Scan(&has); err != nil {
			return err
		}
		skippedParts[SkippedCompetitions] = has
		return nil
	case CopyWrite:
		if s.copyCompetitions == nil {
			return errNoCompetitionCopier
		}
		err := s.copyCompetitions(ctx, tx, plan.Source, plan.Event.ID, plan.Shift.Time, plan.By)
		if subjectlock.IsInactiveAccountReference(err) {
			return ErrForbidden
		}
		return err
	default:
		return nil
	}
}
