package certificate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Automatic attendance finalization (docs/certificate-rule.md, SPEC §9): an
// hour after an Event's last live Session ends, core closes its attendance
// as Finalize does and queues the eligible Tickets' certificates; the
// issuance worker issues and mails them.

const (
	// AutoFinalizeDelay is how long after the last live Session ends the
	// attendance closes by itself (K6).
	AutoFinalizeDelay = time.Hour
	// AutoFinalizeWindow bounds the schedule, as the feedback mail's
	// AutoWindow does: an Event whose due time is older than this is not
	// closed by itself (one that came back from a cancellation, or got its
	// switch on, long after it ended); its organizer closes it by hand.
	AutoFinalizeWindow = 7 * 24 * time.Hour
)

// Who closed an Event's attendance (certificate_event_state.finalized_source).
const (
	FinalizedManual = "manual"
	FinalizedAuto   = "auto"
)

// Finalization is an Event's closed attendance.
type Finalization struct {
	At time.Time
	// By is who closed it; nil when core's clock did (Source auto) or the
	// account is gone.
	By     *uuid.UUID
	Source string
}

// AutoFinalizeStore plans the automatic finalization. PostgresStore has it;
// the memory store has none.
type AutoFinalizeStore interface {
	// AutoFinalizeDue calls each with every Event due at now, one after
	// the other, holding the pass's advisory lock until it returns. A
	// pass that finds the lock taken (another replica's) calls nothing and
	// answers false.
	AutoFinalizeDue(ctx context.Context, now time.Time, each func(eventID uuid.UUID)) (bool, error)
}

// AutoFinalizedMailer tells an Event's publisher that its attendance closed
// by itself and how many certificates it queued (S6). The certificate
// Mailer may implement it; one that does not sends no such mail.
type AutoFinalizedMailer interface {
	CertificateAutoFinalized(ctx context.Context, recipientEmail, fullName string, vars map[string]string)
}

// AutoFinalizeResult is one pass.
type AutoFinalizeResult struct {
	// Locked is false when another replica held the pass.
	Locked bool
	// Finalized counts the Events whose attendance this pass closed.
	Finalized int
	// Errors are the Events it could not close, one error each, naming the
	// Event and the class of the failure (no database text). The next pass
	// tries them again.
	Errors []error
}

// autoFinalizeMetrics are the pass's counters on /v1/metrics.
type autoFinalizeMetrics struct {
	finalized atomic.Int64
	errors    atomic.Int64
}

// Prometheus is the automatic finalization's counters in Prometheus' text
// format. No Event or person appears.
func (s *service) Prometheus() string {
	return fmt.Sprintf("# TYPE core_certificate_auto_finalized_total counter\ncore_certificate_auto_finalized_total %d\n"+
		"# TYPE core_certificate_auto_finalize_errors_total counter\ncore_certificate_auto_finalize_errors_total %d\n",
		s.autoMetrics.finalized.Load(), s.autoMetrics.errors.Load())
}

// AutoFinalize closes, at now, the attendance of every Event due: published
// and current, the rule issuing, its switch on, not closed yet, with a live
// Session, AutoFinalizeDelay past the last live Session's end and within
// AutoFinalizeWindow. Each goes as Finalize, but by the system: no one
// requested it and the source is auto. An Event that fails is left for the
// next pass; one closed meanwhile (by hand, or by another replica) is passed
// over.
func (s *service) AutoFinalize(ctx context.Context, now time.Time) (AutoFinalizeResult, error) {
	if s.autoFinalize == nil || s.jobs == nil || s.templates == nil {
		return AutoFinalizeResult{}, ErrInvalid
	}
	var out AutoFinalizeResult
	locked, err := s.autoFinalize.AutoFinalizeDue(ctx, now, func(eventID uuid.UUID) {
		closed, err := s.autoFinalizeEvent(ctx, eventID, now)
		switch {
		case err != nil:
			s.autoMetrics.errors.Add(1)
			out.Errors = append(out.Errors, fmt.Errorf("event %s: %s", eventID, errorClass(err)))
		case closed:
			s.autoMetrics.finalized.Add(1)
			out.Finalized++
		}
	})
	out.Locked = locked
	if err != nil {
		return out, fmt.Errorf("certificate auto finalize: %s", databaseErrorClass(err))
	}
	return out, nil
}

// autoFinalizeEvent closes one due Event's attendance: false, nil when it no
// longer is due (it changed since the plan, or was closed meanwhile).
func (s *service) autoFinalizeEvent(ctx context.Context, eventID uuid.UUID, now time.Time) (bool, error) {
	ev, err := s.events.Get(ctx, eventID)
	if errors.Is(mapEventError(err), ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ev.Status != event.StatusPublished || ev.ArchivedAt != nil || ev.AttendanceRule == RuleNone || !ev.AttendanceAutoFinalize {
		return false, nil
	}
	attendance, err := s.attendance.EventAttendance(ctx, eventID)
	if err != nil {
		return false, err
	}
	if attendance.LastSessionEnd == nil || attendance.LastSessionEnd.Add(AutoFinalizeDelay).After(now) {
		return false, nil
	}
	resolved, err := s.resolveForEvent(ctx, ev)
	if err != nil {
		return false, err
	}
	batch, err := s.jobs.CreateBatch(ctx, Batch{
		ID: uuid.New(), EventID: ev.ID, TemplateVersionID: resolved.Version.ID, TemplateSource: resolved.Source,
		Reason: "finalization", Status: "queued", FinalizedSource: FinalizedAuto,
	}, finalizationTickets(ev, attendance))
	if errors.Is(err, ErrConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.mailAutoFinalized(ctx, ev, batch.TotalCount)
	return true, nil
}

// finalizationTickets are the Tickets a finalization queues: eligible and
// without an active certificate (a revoked one does not hold one back, as
// before).
func finalizationTickets(ev event.Event, a Attendance) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(a.Tickets))
	for _, item := range a.Tickets {
		if item.Active != nil || !ticketEligible(ev, a, item) {
			continue
		}
		ids = append(ids, item.TicketID)
	}
	return ids
}

// mailAutoFinalized tells who published the Event that its attendance closed
// by itself and how many certificates are on their way (S6). Nothing goes
// when the mailer cannot send it, no one is known to have published the
// Event, or that account is gone.
func (s *service) mailAutoFinalized(ctx context.Context, ev event.Event, queued int) {
	mailer, ok := s.mail.(AutoFinalizedMailer)
	if !ok || ev.PublishedBy == nil || s.users == nil {
		return
	}
	u, err := s.users.Get(ctx, *ev.PublishedBy)
	if err != nil || strings.TrimSpace(u.Email) == "" || (u.AccountState != "" && u.AccountState != user.AccountActive) {
		return
	}
	mailer.CertificateAutoFinalized(ctx, u.Email, strings.TrimSpace(u.FirstName+" "+u.LastName), map[string]string{
		"FirstName": u.FirstName, "EventName": ev.Name, "EventId": ev.ID.String(),
		"CertificateCount": strconv.Itoa(queued), "OwnerTeam": organizer.Name(ev.OwnerTeam),
	})
}

// finalizeOpen reports whether attendance finalization may run at now: the
// Event is published and has ended or is undated (event.FinalizeOpenAt), or
// its last live Session has ended (K7, S4). The automatic finalization
// waits AutoFinalizeDelay more; the organizer need not.
func finalizeOpen(ev event.Event, a Attendance, now time.Time) bool {
	if ev.FinalizeOpenAt(now) {
		return true
	}
	return ev.Status == event.StatusPublished && a.LastSessionEnd != nil && !a.LastSessionEnd.After(now)
}

// autoFinalizeAt is when the attendance closes by itself, nil when it will
// not: closed already, no rule, the switch off, not published, no live
// Session with an end, or due longer ago than AutoFinalizeWindow.
func autoFinalizeAt(ev event.Event, a Attendance, finalized bool, now time.Time) *time.Time {
	if finalized || ev.AttendanceRule == RuleNone || !ev.AttendanceAutoFinalize || ev.Status != event.StatusPublished || a.LastSessionEnd == nil {
		return nil
	}
	at := a.LastSessionEnd.Add(AutoFinalizeDelay)
	if !at.After(now.Add(-AutoFinalizeWindow)) {
		return nil
	}
	return &at
}

// RescanResult is what a rescan queued: how many Tickets, and the batch
// when there were any.
type RescanResult struct {
	Queued int    `json:"queued"`
	Batch  *Batch `json:"batch,omitempty"`
}

// Rescan ("Uygunları yeniden tara", K8) queues, after the attendance has
// closed, the Tickets that are eligible now and have never had a
// certificate: not one with an active certificate, not one whose
// certificate was revoked (a revocation is deliberate), not one with a job
// queued or running. It opens no batch when there is no one.
func (s *service) Rescan(ctx context.Context, p authz.Principal, eventID uuid.UUID) (RescanResult, error) {
	if s.jobs == nil || s.templates == nil {
		return RescanResult{}, ErrInvalid
	}
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return RescanResult{}, err
	}
	if !s.canIssue(p, ev.OwnerTeam) {
		return RescanResult{}, ErrForbidden
	}
	if err := issuable(ev); err != nil {
		return RescanResult{}, err
	}
	if finalized, err := s.jobs.AttendanceFinalizedAt(ctx, eventID); err != nil {
		return RescanResult{}, err
	} else if finalized == nil {
		return RescanResult{}, ErrConflict
	}
	attendance, err := s.attendance.EventAttendance(ctx, eventID)
	if err != nil {
		return RescanResult{}, err
	}
	ids := make([]uuid.UUID, 0)
	for _, item := range attendance.Tickets {
		if item.Active != nil || item.Revoked != nil || item.Job == JobQueued || !ticketEligible(ev, attendance, item) {
			continue
		}
		ids = append(ids, item.TicketID)
	}
	if len(ids) == 0 {
		return RescanResult{Queued: 0}, nil
	}
	batch, err := s.queueResolved(ctx, p, ev, ids, "rescan", nil)
	if err != nil {
		return RescanResult{}, err
	}
	return RescanResult{Queued: batch.TotalCount, Batch: &batch}, nil
}

// errorClass names a failure for the log without its text: a store error
// by its class (databaseErrorClass), the service's own by its code.
func errorClass(err error) string {
	switch {
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict), errors.Is(err, ErrForbidden):
		return safeErrorCode(err)
	default:
		return databaseErrorClass(err)
	}
}

// MaintainAutoFinalize runs AutoFinalize every interval until ctx is
// cancelled; onError hears a pass that failed and each Event a pass could
// not close. The returned channel closes once the pass in flight has
// returned after that; shutdown waits on it.
func MaintainAutoFinalize(ctx context.Context, svc Service, interval time.Duration, onError func(error)) <-chan struct{} {
	if interval <= 0 {
		interval = time.Minute
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				result, err := svc.AutoFinalize(ctx, time.Now())
				if onError == nil {
					continue
				}
				if err != nil {
					onError(err)
				}
				for _, failed := range result.Errors {
					onError(failed)
				}
			}
		}
	}()
	return done
}
