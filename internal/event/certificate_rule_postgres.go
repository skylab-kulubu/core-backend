package event

import (
	"context"

	"github.com/google/uuid"
)

// SetCertificateRule writes the Event's rule and threshold (a rule written
// here has no ratio), its automatic finalization switch (nil keeps it) and
// the listed Sessions' marks in one transaction:
// ErrNotFound when the Event is not current, ErrInvalid (nothing written)
// when a listed Session is not a current Session of the Event on a current
// day.
func (s *PostgresStore) SetCertificateRule(ctx context.Context, eventID uuid.UUID, rule string, minCount *int, autoFinalize *bool, flags []SessionCertificateFlags) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE events SET attendance_rule = $2, attendance_min_count = $3, attendance_ratio = NULL,
			attendance_auto_finalize = COALESCE($4::boolean, attendance_auto_finalize), updated_at = now()
		WHERE id = $1 AND archived_at IS NULL`, eventID, attendanceRule(rule), minCount, autoFinalize)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if len(flags) > 0 {
		ids := make([]uuid.UUID, len(flags))
		counted := make([]bool, len(flags))
		required := make([]bool, len(flags))
		for i, f := range flags {
			ids[i], counted[i], required[i] = f.ID, f.Counted, f.Required
		}
		tag, err = tx.Exec(ctx, `
			UPDATE sessions s SET certificate_counted = f.counted, certificate_required = f.required
			FROM unnest($2::uuid[], $3::boolean[], $4::boolean[]) AS f(id, counted, required), event_days d
			WHERE s.id = f.id AND d.id = s.event_day_id AND d.event_id = $1
			  AND d.archived_at IS NULL AND s.archived_at IS NULL`, eventID, ids, counted, required)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != int64(len(flags)) {
			return ErrInvalid
		}
	}
	return tx.Commit(ctx)
}

// SetCertificateRule writes as PostgresStore's does.
func (s *MemoryStore) SetCertificateRule(_ context.Context, eventID uuid.UUID, rule string, minCount *int, autoFinalize *bool, flags []SessionCertificateFlags) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok || e.ArchivedAt != nil {
		return ErrNotFound
	}
	for _, f := range flags {
		sess, ok := s.sessions[f.ID]
		if !ok || sess.ArchivedAt != nil {
			return ErrInvalid
		}
		day, ok := s.days[sess.EventDayID]
		if !ok || day.EventID != eventID || day.ArchivedAt != nil {
			return ErrInvalid
		}
	}
	e.AttendanceRule, e.AttendanceMinCount, e.AttendanceRatio = attendanceRule(rule), minCount, nil
	if autoFinalize != nil {
		e.AttendanceAutoFinalize = *autoFinalize
	}
	s.byID[eventID] = e
	for _, f := range flags {
		sess := s.sessions[f.ID]
		sess.CertificateCounted, sess.CertificateRequired = f.Counted, f.Required
		s.sessions[f.ID] = sess
	}
	return nil
}
