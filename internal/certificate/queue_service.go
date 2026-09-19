package certificate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

func (s *service) Summary(ctx context.Context, p authz.Principal, eventID uuid.UUID) (EventSummary, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		return EventSummary{}, mapEventError(err)
	}
	if !s.canReadWorkspace(p, ev.OwnerTeam) {
		return EventSummary{}, ErrForbidden
	}
	resolution, err := s.resolveForEvent(ctx, ev)
	if err != nil {
		return EventSummary{}, err
	}
	tickets, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return EventSummary{}, err
	}
	eligibleCount := 0
	for _, item := range tickets {
		eligible, err := s.eligible(ctx, ev, item)
		if err != nil {
			return EventSummary{}, err
		}
		if eligible {
			eligibleCount++
		}
	}
	certs, err := s.store.ListByEvent(ctx, eventID)
	if err != nil {
		return EventSummary{}, err
	}
	issued, revoked := 0, 0
	for _, item := range certs {
		if item.RevokedAt == nil {
			issued++
		} else {
			revoked++
		}
	}
	var finalized *time.Time
	queued, failed := 0, 0
	if s.jobs != nil {
		finalized, err = s.jobs.AttendanceFinalizedAt(ctx, eventID)
		if err != nil {
			return EventSummary{}, err
		}
		queued, failed, err = s.jobs.JobCounts(ctx, eventID)
		if err != nil {
			return EventSummary{}, err
		}
	}
	return EventSummary{EventID: eventID, AttendanceFinalizedAt: finalized, EligibleCount: eligibleCount, IssuedCount: issued, RevokedCount: revoked, QueuedCount: queued, FailedCount: failed, Resolution: s.projectResolution(p, resolution)}, nil
}

func (s *service) Finalize(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Batch, error) {
	if s.jobs == nil || s.templates == nil {
		return Batch{}, ErrInvalid
	}
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		return Batch{}, mapEventError(err)
	}
	if !s.canIssue(p, ev.OwnerTeam) {
		return Batch{}, ErrForbidden
	}
	if ev.Active {
		return Batch{}, ErrConflict
	}
	if ev.AttendanceRule == RuleNone {
		return Batch{}, ErrInvalid
	}
	if previous, err := s.jobs.AttendanceFinalizedAt(ctx, eventID); err != nil {
		return Batch{}, err
	} else if previous != nil {
		return Batch{}, ErrConflict
	}
	tickets, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return Batch{}, err
	}
	ids := make([]uuid.UUID, 0, len(tickets))
	for _, item := range tickets {
		eligible, err := s.eligible(ctx, ev, item)
		if err != nil {
			return Batch{}, err
		}
		if !eligible {
			continue
		}
		if _, err := s.store.GetActive(ctx, eventID, item.ID); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return Batch{}, err
		}
		ids = append(ids, item.ID)
	}
	return s.queueResolved(ctx, p, ev, ids, "finalization")
}

func (s *service) QueueManual(ctx context.Context, p authz.Principal, eventID, ticketID uuid.UUID) (Batch, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		return Batch{}, mapEventError(err)
	}
	if !s.canIssue(p, ev.OwnerTeam) {
		return Batch{}, ErrForbidden
	}
	item, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return Batch{}, mapTicketError(err)
	}
	if item.EventID != eventID {
		return Batch{}, ErrInvalid
	}
	if _, err := s.store.GetActive(ctx, eventID, ticketID); err == nil {
		return Batch{}, ErrConflict
	} else if !errors.Is(err, ErrNotFound) {
		return Batch{}, err
	}
	return s.queueResolved(ctx, p, ev, []uuid.UUID{ticketID}, "manual")
}

func (s *service) Reissue(ctx context.Context, p authz.Principal, serial string) (Batch, error) {
	cert, _, err := s.store.GetBySerial(ctx, serial)
	if err != nil {
		return Batch{}, err
	}
	resource := authz.Resource{Type: authz.TypeCertificate, OwnerTeam: cert.OwnerTeam}
	if !s.authz.Allow(p, resource, authz.Issue) ||
		(cert.RevokedAt == nil && !s.authz.Allow(p, resource, authz.Revoke)) {
		return Batch{}, ErrForbidden
	}
	ev, err := s.events.Get(ctx, cert.EventID)
	if err != nil {
		return Batch{}, mapEventError(err)
	}
	return s.queueResolved(ctx, p, ev, []uuid.UUID{cert.TicketID}, "reissue")
}

func (s *service) queueResolved(ctx context.Context, p authz.Principal, ev event.Event, ticketIDs []uuid.UUID, reason string) (Batch, error) {
	if s.jobs == nil || s.templates == nil {
		return Batch{}, ErrInvalid
	}
	resolved, err := s.resolveForEvent(ctx, ev)
	if err != nil {
		return Batch{}, err
	}
	batch := Batch{ID: uuid.New(), EventID: ev.ID, TemplateVersionID: resolved.Version.ID, TemplateSource: resolved.Source, Reason: reason, Status: "queued", RequestedBy: principalUUIDPtr(p)}
	return s.jobs.CreateBatch(ctx, batch, ticketIDs)
}

func (s *service) GetBatch(ctx context.Context, p authz.Principal, id uuid.UUID) (Batch, error) {
	if s.jobs == nil {
		return Batch{}, ErrInvalid
	}
	batch, err := s.jobs.GetBatch(ctx, id)
	if err != nil {
		return Batch{}, err
	}
	ev, err := s.events.Get(ctx, batch.EventID)
	if err != nil {
		return Batch{}, mapEventError(err)
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: ev.OwnerTeam}, authz.Read) {
		return Batch{}, ErrForbidden
	}
	return batch, nil
}

func (s *service) ListBatches(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Batch, error) {
	if s.jobs == nil {
		return nil, ErrInvalid
	}
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		return nil, mapEventError(err)
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: ev.OwnerTeam}, authz.Read) {
		return nil, ErrForbidden
	}
	return s.jobs.ListBatches(ctx, eventID)
}

func (s *service) RetryBatch(ctx context.Context, p authz.Principal, id uuid.UUID) (Batch, error) {
	batch, err := s.GetBatch(ctx, p, id)
	if err != nil {
		return Batch{}, err
	}
	ev, err := s.events.Get(ctx, batch.EventID)
	if err != nil {
		return Batch{}, mapEventError(err)
	}
	if !s.canIssue(p, ev.OwnerTeam) {
		return Batch{}, ErrForbidden
	}
	if err := s.jobs.RetryBatch(ctx, id); err != nil {
		return Batch{}, err
	}
	return s.jobs.GetBatch(ctx, id)
}

func (s *service) ProcessNext(ctx context.Context, limit int) (int, error) {
	if s.jobs == nil || s.templates == nil {
		return 0, ErrInvalid
	}
	claimed, err := s.jobs.ClaimJobs(ctx, limit, 2*time.Minute)
	if err != nil {
		return 0, err
	}
	for _, job := range claimed {
		if err := s.processJob(ctx, job); err != nil {
			terminal := job.AttemptCount >= 5 || errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound)
			delay := time.Duration(1<<min(job.AttemptCount, 6)) * time.Minute
			_ = s.jobs.FailJob(ctx, job.ID, safeErrorCode(err), time.Now().UTC().Add(delay), terminal)
		}
	}
	return len(claimed), nil
}

func (s *service) processJob(ctx context.Context, job Job) error {
	batch, err := s.jobs.GetBatch(ctx, job.BatchID)
	if err != nil {
		return err
	}
	replaceSerial := ""
	if active, err := s.store.GetActive(ctx, job.EventID, job.TicketID); err == nil {
		if batch.Reason != "reissue" {
			return s.jobs.CompleteJob(ctx, job.ID, active.ID)
		}
		replaceSerial = active.Serial
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	version, err := s.templates.GetVersion(ctx, batch.TemplateVersionID)
	if err != nil {
		return err
	}
	ev, err := s.events.Get(ctx, job.EventID)
	if err != nil {
		return mapEventError(err)
	}
	item, err := s.tickets.Get(ctx, job.TicketID)
	if err != nil {
		return mapTicketError(err)
	}
	created, err := s.materializeVersion(ctx, ev, item, version, batch, job, replaceSerial)
	if err != nil {
		return err
	}
	if err := s.jobs.CompleteJob(ctx, job.ID, created.ID); err != nil {
		return err
	}
	s.sendMail(ctx, created)
	return nil
}

func (s *service) materializeVersion(ctx context.Context, ev event.Event, item ticket.Ticket, version TemplateVersion, batch Batch, job Job, replaceSerial string) (Certificate, error) {
	if s.render == nil {
		return Certificate{}, ErrInvalid
	}
	name, email, err := s.recipient(ctx, item)
	if err != nil {
		return Certificate{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return Certificate{}, err
	}
	verifyURL := s.verifyURL(serial)
	data := PreviewData{RecipientName: name, EventName: ev.Name, OwnerTeam: ev.OwnerTeam, Serial: serial, IssueDate: time.Now().Format("02.01.2006"), EventDates: eventDates(ev)}
	pdf, err := s.renderLayoutPDF(ctx, version.Layout, data, verifyURL, s.assetsForVersion(version))
	if err != nil {
		return Certificate{}, err
	}
	sum := sha256.Sum256(pdf)
	cert := Certificate{
		ID: uuid.New(), EventID: ev.ID, TicketID: item.ID, OwnerID: item.OwnerID, Serial: serial,
		RecipientName: name, RecipientEmail: email, EventName: ev.Name, OwnerTeam: ev.OwnerTeam,
		VerifyURL: verifyURL, TemplateVersionID: &version.ID, TemplateSource: batch.TemplateSource,
		PDFSHA256: hex.EncodeToString(sum[:]), BatchID: &batch.ID, JobID: &job.ID,
	}
	legacyPDF := pdf
	if s.artifacts != nil {
		cert.PDFKey = "certificates/" + serial + ".pdf"
		if err := s.artifacts.Put(ctx, cert.PDFKey, pdf, "application/pdf"); err != nil {
			return Certificate{}, err
		}
		legacyPDF = nil
	}
	// A reissue keeps the previous credential valid until the replacement PDF is ready,
	// then swaps both records atomically so an insert failure cannot leave no active credential.
	if replaceSerial != "" {
		return s.store.Replace(ctx, replaceSerial, cert, legacyPDF)
	}
	return s.store.Create(ctx, cert, legacyPDF)
}

func eventDates(ev event.Event) string {
	if ev.StartDate == nil && ev.EndDate == nil {
		return ""
	}
	if ev.StartDate == nil {
		return ev.EndDate.Format("02.01.2006")
	}
	if ev.EndDate == nil || ev.StartDate.Equal(*ev.EndDate) {
		return ev.StartDate.Format("02.01.2006")
	}
	return ev.StartDate.Format("02.01.2006") + " – " + ev.EndDate.Format("02.01.2006")
}

func safeErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrInvalid):
		return "invalid_input"
	case errors.Is(err, ErrNotFound):
		return "dependency_not_found"
	case errors.Is(err, ErrConflict):
		return "certificate_conflict"
	default:
		return "processing_failed"
	}
}

func MaintainIssuance(ctx context.Context, svc Service, interval time.Duration, limit int, onError func(error)) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if limit <= 0 {
		limit = 10
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for {
					processed, err := svc.ProcessNext(ctx, limit)
					if err != nil {
						if onError != nil {
							onError(err)
						}
						break
					}
					if processed < limit {
						break
					}
				}
			}
		}
	}()
}
