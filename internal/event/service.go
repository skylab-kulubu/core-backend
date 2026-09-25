package event

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type Service interface {
	CanAssign(p authz.Principal, ownerTeam string) bool
	ProjectFor(p *authz.Principal, in Event) Event
	ProjectAllFor(p *authz.Principal, in []Event) []Event
	List(ctx context.Context, ownerTeam string, activeOnly bool) ([]Event, error)
	ListLifecycle(ctx context.Context, p authz.Principal, ownerTeam string, visibility lifecycle.Visibility) ([]Event, error)
	Get(ctx context.Context, id uuid.UUID) (Event, error)
	Create(ctx context.Context, p authz.Principal, in Event) (Event, error)
	Update(ctx context.Context, p authz.Principal, id uuid.UUID, in Event) (Event, error)
	Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error
	Restore(ctx context.Context, p authz.Principal, id uuid.UUID) (Event, error)
	AddImages(ctx context.Context, p authz.Principal, id uuid.UUID, ids []uuid.UUID) (Event, error)
	RemoveImages(ctx context.Context, p authz.Principal, id uuid.UUID, ids []uuid.UUID) (Event, error)
	ListDays(ctx context.Context, eventID uuid.UUID) ([]Day, error)
	ListDaysLifecycle(ctx context.Context, p authz.Principal, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Day, error)
	GetDay(ctx context.Context, id uuid.UUID) (Day, error)
	CreateDay(ctx context.Context, p authz.Principal, d Day) (Day, error)
	UpdateDay(ctx context.Context, p authz.Principal, id uuid.UUID, d Day) (Day, error)
	DeleteDay(ctx context.Context, p authz.Principal, id uuid.UUID) error
	RestoreDay(ctx context.Context, p authz.Principal, id uuid.UUID) (Day, error)
	ListSessions(ctx context.Context, eventDayID uuid.UUID) ([]Session, error)
	ListSessionsLifecycle(ctx context.Context, p authz.Principal, eventDayID uuid.UUID, visibility lifecycle.Visibility) ([]Session, error)
	GetSession(ctx context.Context, id uuid.UUID) (Session, error)
	CurrentSession(ctx context.Context, eventDayID uuid.UUID, at time.Time) (Current, error)
	CreateSession(ctx context.Context, p authz.Principal, sess Session) (Session, error)
	UpdateSession(ctx context.Context, p authz.Principal, id uuid.UUID, sess Session) (Session, error)
	DeleteSession(ctx context.Context, p authz.Principal, id uuid.UUID) error
	RestoreSession(ctx context.Context, p authz.Principal, id uuid.UUID) (Session, error)
	ListBySeason(ctx context.Context, seasonID uuid.UUID) ([]Event, error)
	AssignSeason(ctx context.Context, p authz.Principal, eventID uuid.UUID, seasonID *uuid.UUID) (Event, error)
}

type service struct {
	store      Store
	authz      authz.Authorizer
	publicBase string
	formLinks  FormLinkSync
}

func NewService(store Store, az authz.Authorizer, publicBase ...string) Service {
	base := ""
	if len(publicBase) > 0 {
		base = publicBase[0]
	}
	return &service{store: store, authz: az, publicBase: base}
}

func (s *service) publish(e Event) Event {
	return withPublicMedia(e, s.publicBase)
}

func (s *service) publishAll(events []Event) []Event {
	return withPublicMediaAll(events, s.publicBase)
}

func resource(ownerTeam string) authz.Resource {
	return authz.Resource{Type: authz.TypeEvent, OwnerTeam: ownerTeam}
}

func (s *service) CanAssign(p authz.Principal, ownerTeam string) bool {
	return s.authz.Allow(p, resource(ownerTeam), authz.Assign)
}

func (s *service) ProjectFor(p *authz.Principal, in Event) Event {
	if p == nil || !s.CanAssign(*p, in.OwnerTeam) {
		in.DoorStaffIDs = nil
	}
	return in
}

func (s *service) ProjectAllFor(p *authz.Principal, in []Event) []Event {
	for i := range in {
		in[i] = s.ProjectFor(p, in[i])
	}
	return in
}

func (s *service) published(e Event, err error) (Event, error) {
	if err != nil {
		return Event{}, err
	}
	return s.publish(e), nil
}

func (s *service) List(ctx context.Context, ownerTeam string, activeOnly bool) ([]Event, error) {
	events, err := s.store.List(ctx, ownerTeam, activeOnly)
	if err != nil {
		return nil, err
	}
	return s.publishAll(events), nil
}

func (s *service) ListLifecycle(ctx context.Context, p authz.Principal, ownerTeam string, visibility lifecycle.Visibility) ([]Event, error) {
	if ownerTeam != "" && !s.authz.Allow(p, resource(ownerTeam), authz.Delete) {
		return nil, ErrForbidden
	}
	events, err := s.store.ListLifecycle(ctx, ownerTeam, visibility)
	if err != nil {
		return nil, err
	}
	visible := make([]Event, 0, len(events))
	for _, item := range events {
		if s.authz.Allow(p, resource(item.OwnerTeam), authz.Delete) {
			visible = append(visible, item)
		}
	}
	return s.publishAll(visible), nil
}

func (s *service) Get(ctx context.Context, id uuid.UUID) (Event, error) {
	e, err := s.store.Get(ctx, id)
	if err != nil {
		return Event{}, err
	}
	return s.publish(e), nil
}

func normalizeAttendance(in Event) (Event, error) {
	rule := strings.TrimSpace(strings.ToLower(in.AttendanceRule))
	if rule == "" {
		rule = "none"
	}
	switch rule {
	case "none", "once":
		in.AttendanceRule = rule
		return in, nil
	case "ratio":
		if in.AttendanceRatio == nil || *in.AttendanceRatio <= 0 || *in.AttendanceRatio > 1 {
			return Event{}, ErrInvalid
		}
		in.AttendanceRule = rule
		return in, nil
	default:
		return Event{}, ErrInvalid
	}
}

func (s *service) Create(ctx context.Context, p authz.Principal, in Event) (Event, error) {
	if in.Name == "" || in.Location == "" {
		return Event{}, ErrInvalid
	}
	in, err := normalizeAttendance(in)
	if err != nil {
		return Event{}, err
	}
	if !s.authz.Allow(p, resource(in.OwnerTeam), authz.Create) {
		return Event{}, ErrForbidden
	}
	if !s.authz.Allow(p, resource(in.OwnerTeam), authz.Assign) {
		in.DoorStaffIDs = nil
	}
	created, err := s.store.Create(ctx, in)
	if err == nil {
		s.syncFormLinks(ctx, created.ID, formLinksOf(created))
	}
	return s.published(created, err)
}

func (s *service) Update(ctx context.Context, p authz.Principal, id uuid.UUID, in Event) (Event, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if !s.authz.Allow(p, resource(existing.OwnerTeam), authz.Update) {
		return Event{}, ErrForbidden
	}
	if in.OwnerTeam != existing.OwnerTeam && !s.authz.Allow(p, resource(in.OwnerTeam), authz.Update) {
		return Event{}, ErrForbidden
	}
	if in.Name == "" || in.Location == "" {
		return Event{}, ErrInvalid
	}
	in, err = normalizeAttendance(in)
	if err != nil {
		return Event{}, err
	}
	in.ID = existing.ID
	if in.SeasonID == nil {
		in.SeasonID = existing.SeasonID
	}
	if !s.authz.Allow(p, resource(existing.OwnerTeam), authz.Assign) || in.DoorStaffIDs == nil {
		in.DoorStaffIDs = existing.DoorStaffIDs
	}
	if in.ExtraFormURLs == nil {
		in.ExtraFormURLs = existing.ExtraFormURLs
		if in.FormAlias == "" {
			in.FormAlias = existing.FormAlias
		}
	}
	if in.MailListID == nil {
		in.MailListID = existing.MailListID
	}
	updated, err := s.store.Update(ctx, in)
	if err == nil {
		s.syncFormLinks(ctx, updated.ID, formLinksOf(updated))
	}
	return s.published(updated, err)
}

func (s *service) Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	existing, err := s.store.GetIncludingArchived(ctx, id)
	if err != nil {
		return err
	}
	if !s.authz.Allow(p, resource(existing.OwnerTeam), authz.Delete) {
		return ErrForbidden
	}
	if err := s.store.Archive(ctx, id, lifecycle.ActorID(p.ID)); err != nil {
		return err
	}
	s.syncFormLinks(ctx, id, nil)
	return nil
}

func (s *service) Restore(ctx context.Context, p authz.Principal, id uuid.UUID) (Event, error) {
	existing, err := s.store.GetIncludingArchived(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if !s.authz.Allow(p, resource(existing.OwnerTeam), authz.Delete) {
		return Event{}, ErrForbidden
	}
	if err := s.store.Restore(ctx, id); err != nil {
		return Event{}, err
	}
	restored, err := s.Get(ctx, id)
	if err == nil {
		s.syncFormLinks(ctx, restored.ID, formLinksOf(restored))
	}
	return restored, err
}

func (s *service) AddImages(ctx context.Context, p authz.Principal, id uuid.UUID, ids []uuid.UUID) (Event, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if !s.authz.Allow(p, resource(existing.OwnerTeam), authz.Update) {
		return Event{}, ErrForbidden
	}
	return s.published(s.store.AddImages(ctx, id, ids))
}

func (s *service) RemoveImages(ctx context.Context, p authz.Principal, id uuid.UUID, ids []uuid.UUID) (Event, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if !s.authz.Allow(p, resource(existing.OwnerTeam), authz.Update) {
		return Event{}, ErrForbidden
	}
	return s.published(s.store.RemoveImages(ctx, id, ids))
}

func (s *service) ownerResource(owner string, t authz.Type) authz.Resource {
	return authz.Resource{Type: t, OwnerTeam: owner}
}

func (s *service) eventOwner(ctx context.Context, eventID uuid.UUID) (string, error) {
	ev, err := s.store.Get(ctx, eventID)
	if err != nil {
		return "", err
	}
	return ev.OwnerTeam, nil
}

func (s *service) ListDays(ctx context.Context, eventID uuid.UUID) ([]Day, error) {
	if _, err := s.store.Get(ctx, eventID); err != nil {
		return nil, err
	}
	return s.store.ListDays(ctx, eventID)
}

func (s *service) ListDaysLifecycle(ctx context.Context, p authz.Principal, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Day, error) {
	ev, err := s.store.GetIncludingArchived(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if !s.authz.Allow(p, s.ownerResource(ev.OwnerTeam, authz.TypeEventDay), authz.Delete) {
		return nil, ErrForbidden
	}
	return s.store.ListDaysLifecycle(ctx, eventID, visibility)
}

func (s *service) GetDay(ctx context.Context, id uuid.UUID) (Day, error) {
	return s.store.GetDay(ctx, id)
}

func (s *service) CreateDay(ctx context.Context, p authz.Principal, d Day) (Day, error) {
	if d.EventID == uuid.Nil {
		return Day{}, ErrInvalid
	}
	owner, err := s.eventOwner(ctx, d.EventID)
	if err != nil {
		return Day{}, err
	}
	if !s.authz.Allow(p, s.ownerResource(owner, authz.TypeEventDay), authz.Create) {
		return Day{}, ErrForbidden
	}
	return s.store.CreateDay(ctx, d)
}

func (s *service) UpdateDay(ctx context.Context, p authz.Principal, id uuid.UUID, d Day) (Day, error) {
	existing, err := s.store.GetDay(ctx, id)
	if err != nil {
		return Day{}, err
	}
	owner, err := s.eventOwner(ctx, existing.EventID)
	if err != nil {
		return Day{}, err
	}
	if !s.authz.Allow(p, s.ownerResource(owner, authz.TypeEventDay), authz.Update) {
		return Day{}, ErrForbidden
	}
	d.ID = existing.ID
	d.EventID = existing.EventID
	return s.store.UpdateDay(ctx, d)
}

func (s *service) DeleteDay(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	existing, err := s.store.GetDayIncludingArchived(ctx, id)
	if err != nil {
		return err
	}
	ev, err := s.store.GetIncludingArchived(ctx, existing.EventID)
	if err != nil {
		return err
	}
	if !s.authz.Allow(p, s.ownerResource(ev.OwnerTeam, authz.TypeEventDay), authz.Delete) {
		return ErrForbidden
	}
	return s.store.ArchiveDay(ctx, id, lifecycle.ActorID(p.ID))
}

func (s *service) RestoreDay(ctx context.Context, p authz.Principal, id uuid.UUID) (Day, error) {
	existing, err := s.store.GetDayIncludingArchived(ctx, id)
	if err != nil {
		return Day{}, err
	}
	ev, err := s.store.GetIncludingArchived(ctx, existing.EventID)
	if err != nil {
		return Day{}, err
	}
	if !s.authz.Allow(p, s.ownerResource(ev.OwnerTeam, authz.TypeEventDay), authz.Delete) {
		return Day{}, ErrForbidden
	}
	if ev.ArchivedAt != nil {
		return Day{}, ErrConflict
	}
	if err := s.store.RestoreDay(ctx, id); err != nil {
		return Day{}, err
	}
	return s.store.GetDay(ctx, id)
}

func (s *service) ListSessions(ctx context.Context, eventDayID uuid.UUID) ([]Session, error) {
	if _, err := s.store.GetDay(ctx, eventDayID); err != nil {
		return nil, err
	}
	return s.store.ListSessions(ctx, eventDayID)
}

func (s *service) ListSessionsLifecycle(ctx context.Context, p authz.Principal, eventDayID uuid.UUID, visibility lifecycle.Visibility) ([]Session, error) {
	day, err := s.store.GetDayIncludingArchived(ctx, eventDayID)
	if err != nil {
		return nil, err
	}
	ev, err := s.store.GetIncludingArchived(ctx, day.EventID)
	if err != nil {
		return nil, err
	}
	if !s.authz.Allow(p, s.ownerResource(ev.OwnerTeam, authz.TypeSession), authz.Delete) {
		return nil, ErrForbidden
	}
	return s.store.ListSessionsLifecycle(ctx, eventDayID, visibility)
}

func (s *service) GetSession(ctx context.Context, id uuid.UUID) (Session, error) {
	return s.store.GetSession(ctx, id)
}

func (s *service) CurrentSession(ctx context.Context, eventDayID uuid.UUID, at time.Time) (Current, error) {
	if _, err := s.store.GetDay(ctx, eventDayID); err != nil {
		return Current{}, err
	}
	sessions, err := s.store.ListSessions(ctx, eventDayID)
	if err != nil {
		return Current{}, err
	}
	return ResolveCurrent(sessions, at), nil
}

func (s *service) sessionOwner(ctx context.Context, eventDayID uuid.UUID) (string, error) {
	day, err := s.store.GetDay(ctx, eventDayID)
	if err != nil {
		return "", err
	}
	return s.eventOwner(ctx, day.EventID)
}

func (s *service) CreateSession(ctx context.Context, p authz.Principal, sess Session) (Session, error) {
	if err := validateSession(sess); err != nil {
		return Session{}, ErrInvalid
	}
	owner, err := s.sessionOwner(ctx, sess.EventDayID)
	if err != nil {
		return Session{}, err
	}
	if !s.authz.Allow(p, s.ownerResource(owner, authz.TypeSession), authz.Create) {
		return Session{}, ErrForbidden
	}
	return s.store.CreateSession(ctx, sess)
}

func (s *service) UpdateSession(ctx context.Context, p authz.Principal, id uuid.UUID, sess Session) (Session, error) {
	existing, err := s.store.GetSession(ctx, id)
	if err != nil {
		return Session{}, err
	}
	owner, err := s.sessionOwner(ctx, existing.EventDayID)
	if err != nil {
		return Session{}, err
	}
	if !s.authz.Allow(p, s.ownerResource(owner, authz.TypeSession), authz.Update) {
		return Session{}, ErrForbidden
	}
	sess.ID = existing.ID
	sess.EventDayID = existing.EventDayID
	if err := validateSession(sess); err != nil {
		return Session{}, ErrInvalid
	}
	return s.store.UpdateSession(ctx, sess)
}

func (s *service) DeleteSession(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	existing, err := s.store.GetSessionIncludingArchived(ctx, id)
	if err != nil {
		return err
	}
	day, err := s.store.GetDayIncludingArchived(ctx, existing.EventDayID)
	if err != nil {
		return err
	}
	ev, err := s.store.GetIncludingArchived(ctx, day.EventID)
	if err != nil {
		return err
	}
	if !s.authz.Allow(p, s.ownerResource(ev.OwnerTeam, authz.TypeSession), authz.Delete) {
		return ErrForbidden
	}
	return s.store.ArchiveSession(ctx, id, lifecycle.ActorID(p.ID))
}

func (s *service) RestoreSession(ctx context.Context, p authz.Principal, id uuid.UUID) (Session, error) {
	existing, err := s.store.GetSessionIncludingArchived(ctx, id)
	if err != nil {
		return Session{}, err
	}
	day, err := s.store.GetDayIncludingArchived(ctx, existing.EventDayID)
	if err != nil {
		return Session{}, err
	}
	ev, err := s.store.GetIncludingArchived(ctx, day.EventID)
	if err != nil {
		return Session{}, err
	}
	if !s.authz.Allow(p, s.ownerResource(ev.OwnerTeam, authz.TypeSession), authz.Delete) {
		return Session{}, ErrForbidden
	}
	if ev.ArchivedAt != nil || day.ArchivedAt != nil {
		return Session{}, ErrConflict
	}
	if err := validateSession(existing); err != nil {
		return Session{}, err
	}
	if err := s.store.RestoreSession(ctx, id); err != nil {
		return Session{}, err
	}
	return s.store.GetSession(ctx, id)
}

func validateSession(sess Session) error {
	if sess.EventDayID == uuid.Nil || sess.Title == "" || sess.SpeakerName == "" || sess.SessionType == "" {
		return ErrInvalid
	}
	if sess.StartTime != nil && sess.EndTime != nil && !sess.EndTime.After(*sess.StartTime) {
		return ErrInvalid
	}
	return nil
}

func (s *service) ListBySeason(ctx context.Context, seasonID uuid.UUID) ([]Event, error) {
	events, err := s.store.ListBySeason(ctx, seasonID)
	if err != nil {
		return nil, err
	}
	return s.publishAll(events), nil
}

func (s *service) AssignSeason(ctx context.Context, p authz.Principal, eventID uuid.UUID, seasonID *uuid.UUID) (Event, error) {
	existing, err := s.store.Get(ctx, eventID)
	if err != nil {
		return Event{}, err
	}
	if !s.authz.Allow(p, resource(existing.OwnerTeam), authz.Update) {
		return Event{}, ErrForbidden
	}
	return s.published(s.store.SetSeason(ctx, eventID, seasonID))
}
