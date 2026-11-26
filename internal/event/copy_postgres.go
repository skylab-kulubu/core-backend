package event

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
			registration_closes_at, feedback_mode, feedback_auto_send,
			location_lat, location_lng, location_address, attendance_min_count, attendance_auto_finalize,
			waitlist_enabled, entry_mode, attendance_welcome_mail, attendance_recorded_mail,
			external_registration_url, attendee_note
		) VALUES ($2, $3, $4, $5, $6, $7, $1, $8, $9, $10, `+copySeasonSQL+`,
			$11, $12, 'draft', $13, $14, $15, $16, $17, $18, $19, $20, $21, COALESCE($22::boolean, true), $23,
			COALESCE($24::boolean, true), COALESCE($25::boolean, true), $26, COALESCE($27::text, ''))
		ON CONFLICT (id) DO NOTHING`,
		e.StartDate, e.ID, e.Name, e.Description, e.Location, e.OwnerTeam, e.Capacity,
		e.EndDate, e.Ranked, e.PrizeInfo, attendanceRule(e.AttendanceRule), e.AttendanceRatio,
		plan.Source, e.RegistrationClosesAt, string(feedbackMode(e.FeedbackMode)), e.FeedbackAutoSend,
		e.LocationLat, e.LocationLng, e.LocationAddress, e.AttendanceMinCount, e.AttendanceAutoFinalize,
		e.WaitlistEnabled, e.EntryMode, e.AttendanceWelcomeMail, e.AttendanceRecordedMail,
		e.ExternalRegistrationURL, e.AttendeeNote)
	if err != nil {
		return Event{}, nil, err
	}
	if tag.RowsAffected() == 0 {
		return Event{}, nil, ErrConflict
	}
	if plan.AttendeeNote == CopyRefused {
		skippedParts[SkippedAttendeeNote] = true
	}
	if err := copyCover(ctx, tx, plan, skippedParts); err != nil {
		return Event{}, nil, err
	}
	if err := copyMap(ctx, tx, plan); err != nil {
		return Event{}, nil, err
	}
	if err := s.copyDoorStaff(ctx, tx, plan, skippedParts); err != nil {
		return Event{}, nil, err
	}
	if err := copyCoOrganizers(ctx, tx, plan, skippedParts); err != nil {
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

// copyDoorStaff copies the source's door staff whose accounts are active,
// and queues the door staff mail of each (docs/door-staff-mail.md): they are
// new to the copy's door staff.
func (s *PostgresStore) copyDoorStaff(ctx context.Context, tx pgx.Tx, plan CopyPlan, skippedParts map[string]bool) error {
	switch plan.DoorStaff {
	case CopyRefused:
		var has bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_door_staff WHERE event_id = $1)`, plan.Source).Scan(&has); err != nil {
			return err
		}
		skippedParts[SkippedDoorStaff] = has
		return nil
	case CopyWrite:
		rows, err := tx.Query(ctx, `
			INSERT INTO event_door_staff (event_id, user_id)
			SELECT $2, staff.user_id FROM event_door_staff staff
			JOIN users u ON u.id = staff.user_id AND u.account_state = 'active'
			WHERE staff.event_id = $1
				AND NOT EXISTS (SELECT 1 FROM account_deletion_requests r WHERE r.subject_id = staff.user_id)
			RETURNING user_id`,
			plan.Source, plan.Event.ID)
		var added []uuid.UUID
		if err == nil {
			added, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		}
		if subjectlock.IsInactiveAccountReference(err) {
			return ErrForbidden
		}
		if err != nil {
			return err
		}
		return s.queueDoorStaffMail(ctx, tx, plan.Event.ID, added)
	default:
		return nil
	}
}

// copyCoOrganizers writes the copy's Co-organizer teams with their choices
// and, with CopyWrite, the source's Co-organizer people whose accounts are
// active (docs/co-organizers.md); who copies is who added them.
func copyCoOrganizers(ctx context.Context, tx pgx.Tx, plan CopyPlan, skippedParts map[string]bool) error {
	for _, team := range plan.CoOrganizerTeams {
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_co_organizers (event_id, team_code, management, visibility, added_by)
			VALUES ($1, $2, $3, $4, $5)`,
			plan.Event.ID, team.Code, string(team.Management), string(team.Visibility), plan.By); err != nil {
			return err
		}
	}
	switch plan.CoOrganizerPeople {
	case CopyRefused:
		var has bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_co_organizers WHERE event_id = $1 AND user_id IS NOT NULL)`, plan.Source).Scan(&has); err != nil {
			return err
		}
		skippedParts[SkippedCoOrganizerPeople] = has
	case CopyWrite:
		_, err := tx.Exec(ctx, `
			INSERT INTO event_co_organizers (event_id, user_id, added_by)
			SELECT $2, c.user_id, $3 FROM event_co_organizers c
			JOIN users u ON u.id = c.user_id AND u.account_state = 'active'
			WHERE c.event_id = $1
				AND NOT EXISTS (SELECT 1 FROM account_deletion_requests r WHERE r.subject_id = c.user_id)`,
			plan.Source, plan.Event.ID, plan.By)
		if subjectlock.IsInactiveAccountReference(err) {
			return ErrForbidden
		}
		return err
	}
	return nil
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
			SELECT title, speaker_name, speaker_linkedin, description, start_time, end_time, order_index, session_type, feedback_auto_send,
				certificate_counted, certificate_required
			FROM sessions WHERE event_day_id = $1 AND archived_at IS NULL
			ORDER BY order_index, start_time NULLS LAST, id`, day.ID)
		if err != nil {
			return err
		}
		sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) {
			var sess Session
			err := row.Scan(&sess.Title, &sess.SpeakerName, &sess.SpeakerLinkedin, &sess.Description,
				&sess.StartTime, &sess.EndTime, &sess.OrderIndex, &sess.SessionType, &sess.FeedbackAutoSend,
				&sess.CertificateCounted, &sess.CertificateRequired)
			return sess, err
		})
		if err != nil {
			return err
		}
		for _, sess := range sessions {
			if _, err := tx.Exec(ctx, `
				INSERT INTO sessions (id, event_day_id, title, speaker_name, speaker_linkedin, description,
					start_time, end_time, order_index, session_type, cancelled, feedback_auto_send,
					certificate_counted, certificate_required)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, false, $11, $12, $13)`,
				uuid.New(), dayID, sess.Title, sess.SpeakerName, sess.SpeakerLinkedin, sess.Description,
				plan.Shift.At(sess.StartTime), plan.Shift.At(sess.EndTime), sess.OrderIndex, sess.SessionType, sess.FeedbackAutoSend,
				sess.CertificateCounted, sess.CertificateRequired); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyTemplateBinding brings the source's certificate template along
// (docs/certificate-event-templates.md#copy-and-team-change):
//
//   - Its Event template, when it has one, becomes the copy's own: a new
//     template of the copy with the same draft and, when it was published,
//     its latest version as version 1 (the same layout, asset copies and
//     checksum; nothing is rendered again). The copy is bound to it when
//     the source was bound to its Event template.
//   - A binding to a team or club template is copied when the copier may
//     bind and the template may serve the copy's Owner team (a current
//     template of no team or of that team, as the binding rule says).
//
// What is left out is named skipped: the copier asked not to, may not bind,
// the team template does not fit, or the database refused the Event
// template's Media (one deleted since it was published).
func copyTemplateBinding(ctx context.Context, tx pgx.Tx, plan CopyPlan, skippedParts map[string]bool) error {
	var boundID *uuid.UUID
	var boundOwn, fits bool
	err := tx.QueryRow(ctx, `
		SELECT b.template_id, t.event_id IS NOT DISTINCT FROM $1::uuid,
			t.archived_at IS NULL AND t.event_id IS NULL AND upper(btrim(t.owner_team)) IN ('', upper(btrim($2)))
		FROM certificate_template_bindings b
		JOIN certificate_templates t ON t.id = b.template_id
		WHERE b.scope = 'event' AND b.scope_key = $1::uuid::text`, plan.Source, plan.Event.OwnerTeam).Scan(&boundID, &boundOwn, &fits)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	ownCopy, err := copyEventTemplate(ctx, tx, plan, skippedParts)
	if err != nil {
		return err
	}
	switch {
	case boundID == nil:
		return nil
	case boundOwn:
		if ownCopy == nil {
			return nil
		}
		boundID = ownCopy
	case plan.CertificateTemplate != CopyWrite || !fits:
		skippedParts[SkippedCertificateTemplate] = true
		return nil
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO certificate_template_bindings (id, scope, scope_key, template_id, updated_by)
		VALUES ($1, 'event', $2, $3, (SELECT id FROM users WHERE id = $4))`,
		uuid.New(), plan.Event.ID.String(), *boundID, plan.By)
	if subjectlock.IsInactiveAccountReference(err) {
		return ErrForbidden
	}
	return err
}

// copyEventTemplate copies the source's current Event template into a new
// one of the copy, in a savepoint, and answers its id when the copy has a
// published version to be bound to; nil when the source has none, it was a
// draft only, or it was left out (named skipped).
func copyEventTemplate(ctx context.Context, tx pgx.Tx, plan CopyPlan, skippedParts map[string]bool) (*uuid.UUID, error) {
	var sourceID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM certificate_templates WHERE event_id = $1 AND archived_at IS NULL`, plan.Source).Scan(&sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if plan.EventTemplate != CopyWrite {
		skippedParts[SkippedCertificateTemplate] = true
		return nil, nil
	}
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	id := uuid.New()
	// The design's reference and edit address go along only for who may
	// read the source template (EventTemplateSourceLinks).
	_, err = savepoint.Exec(ctx, `
		INSERT INTO certificate_templates (id, name, owner_team, event_id, source_kind, source_ref, source_edit_url, draft_layout, created_by)
		SELECT $1, $2, upper(btrim($3)), $4, source_kind,
			CASE WHEN $6::boolean THEN source_ref ELSE '' END,
			CASE WHEN $6::boolean THEN source_edit_url ELSE '' END,
			draft_layout, (SELECT id FROM users WHERE id = $7)
		FROM certificate_templates WHERE id = $5`,
		id, copyTemplateName(plan.Event.Name), plan.Event.OwnerTeam, plan.Event.ID, sourceID, plan.EventTemplateSourceLinks, plan.By)
	if err == nil {
		var tag pgconn.CommandTag
		tag, err = savepoint.Exec(ctx, `
			INSERT INTO certificate_template_versions (id, template_id, version, layout, asset_manifest, checksum, published_by, asset_serving_policy_applied)
			SELECT $1, $2, 1, layout, asset_manifest, checksum, (SELECT id FROM users WHERE id = $4), asset_serving_policy_applied
			FROM certificate_template_versions WHERE template_id = $3
			ORDER BY version DESC LIMIT 1`, uuid.New(), id, sourceID, plan.By)
		if err == nil && tag.RowsAffected() == 0 {
			// A draft only: copied, bound to nothing.
			return nil, savepoint.Commit(ctx)
		}
	}
	if templateMediaRefused(err) {
		skippedParts[SkippedCertificateTemplate] = true
		return nil, savepoint.Rollback(ctx)
	}
	if subjectlock.IsInactiveAccountReference(err) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	return &id, savepoint.Commit(ctx)
}

// templateMediaRefused reports whether the database refused a template's
// Media: one no longer current (require_current_certificate_layout_media)
// or one the Media attachment does not take.
func templateMediaRefused(err error) bool {
	if _, refused := media.DatabaseLinkRefusal(err); refused {
		return true
	}
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503" && strings.HasSuffix(pg.Message, "is not current")
}

// copyTemplateName is the copy's Event template's name, "<Event name>
// sertifikası", within the 160 bytes a template name takes (as the
// certificate package names a new Event template).
func copyTemplateName(eventName string) string {
	const suffix = " sertifikası"
	name := strings.TrimSpace(eventName)
	if name == "" {
		return "Etkinlik sertifikası"
	}
	if limit := 160 - len(suffix); len(name) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = strings.TrimSpace(name[:cut])
	}
	return name + suffix
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

// copyMap gives the copy its source's map when it has the same point, in a
// savepoint: a map the database refuses to link (its Media being purged)
// is left out, and the worker draws the copy one (docs/event-map.md).
func copyMap(ctx context.Context, tx pgx.Tx, plan CopyPlan) error {
	if plan.Event.LocationLat == nil {
		return nil
	}
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	_, err = savepoint.Exec(ctx, `UPDATE events c SET map_media_id = s.map_media_id, map_retry_at = NULL
		FROM events s
		WHERE c.id = $1 AND s.id = $2 AND s.map_media_id IS NOT NULL
		  AND s.location_lat = c.location_lat AND s.location_lng = c.location_lng`, plan.Event.ID, plan.Source)
	if _, refused := media.DatabaseLinkRefusal(err); refused {
		return savepoint.Rollback(ctx)
	}
	if err != nil {
		return err
	}
	return savepoint.Commit(ctx)
}
