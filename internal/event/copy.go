package event

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// Parts of a copy that can be left out, as the copy's answer names them
// (skipped): the copier may not write them, or a rule refused them.
const (
	SkippedCertificateTemplate = "certificateTemplate"
	SkippedDoorStaff           = "doorStaff"
	SkippedCover               = "cover"
	SkippedCompetitions        = "competitions"
)

// skippedOrder is the order the answer lists skipped parts in.
var skippedOrder = []string{SkippedCertificateTemplate, SkippedDoorStaff, SkippedCover, SkippedCompetitions}

// CopyInput is what POST /v1/events/{id}/copy asks for (ADR 0066, ticket
// 06): the copy's id (the client makes it, so a retried request finds the
// copy it made), its name and its new start.
type CopyInput struct {
	ID                  uuid.UUID
	Name                string
	StartDate           *time.Time
	IncludeDoorStaff    bool
	IncludeCompetitions bool
}

// Copied is a copy and the parts of its source it left out.
type Copied struct {
	Event   Event    `json:"event"`
	Skipped []string `json:"skipped"`
}

// CopyPart is what a copy does with one optional part of its source.
type CopyPart int

const (
	// CopyNot leaves the part out: nobody asked for it.
	CopyNot CopyPart = iota
	// CopyRefused leaves it out because the copier may not write it: the
	// answer names it skipped when the source has it.
	CopyRefused
	// CopyWrite copies it; the store names it skipped when a rule it
	// checks refuses it.
	CopyWrite
)

// CopyPlan is the copy the service decided on, which the store writes in
// one transaction.
type CopyPlan struct {
	// Source is the Event copied.
	Source uuid.UUID
	// Event is the copy: its id, name, Owner team, dates and settings. Its
	// season is the store's to find (the one its new start falls in); its
	// cover is written only with Cover set to CopyWrite.
	Event Event
	// Shift moves the source's days, Sessions and Competition stages.
	Shift Shift
	// By is who copies, nil when unknown.
	By                  *uuid.UUID
	DoorStaff           CopyPart
	Cover               CopyPart
	CertificateTemplate CopyPart
	Competitions        CopyPart
}

// skipped puts the parts in the answer's order, each once, never nil.
func skipped(parts map[string]bool) []string {
	out := make([]string, 0, len(parts))
	for _, part := range skippedOrder {
		if parts[part] {
			out = append(out, part)
		}
	}
	return out
}

// copyZone is the time zone a copy's dates move in: the club's wall clock.
// Türkiye has kept one offset since 2016, so a wall-clock move is the same
// as a move by the duration; the zone is named so that it stays right if
// that changes.
var copyZone = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Istanbul"); err == nil {
		return loc
	}
	return time.FixedZone("Europe/Istanbul", 3*60*60)
}()

// Shift moves a copy's dates by the wall-clock distance between the
// source's anchor and the copy's start (spec §3.2): durations and gaps stay
// as they were.
type Shift struct {
	by time.Duration
}

// wall is t's Europe/Istanbul wall clock, as a UTC time.
func wall(t time.Time) time.Time {
	w := t.In(copyZone)
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC)
}

// NewShift is the move that takes anchor to start. A nil anchor (the source
// has no date at all) moves nothing.
func NewShift(anchor *time.Time, start time.Time) Shift {
	if anchor == nil {
		return Shift{}
	}
	return Shift{by: wall(start).Sub(wall(*anchor))}
}

// Time is t moved.
func (s Shift) Time(t time.Time) time.Time {
	w := wall(t).Add(s.by)
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), copyZone)
}

// At is t moved; nil stays nil.
func (s Shift) At(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	moved := s.Time(*t)
	return &moved
}

// anchor is the date the copy's start replaces: the source's start, else
// its first current day's start, else its first current Session's start;
// nil when it has none.
func (s *service) anchor(ctx context.Context, source Event) (*time.Time, error) {
	if source.StartDate != nil {
		return source.StartDate, nil
	}
	days, err := s.store.ListDays(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	var first *time.Time
	for _, day := range days {
		first = earliest(first, day.StartDate)
	}
	if first != nil {
		return first, nil
	}
	for _, day := range days {
		sessions, err := s.store.ListSessions(ctx, day.ID)
		if err != nil {
			return nil, err
		}
		for _, sess := range sessions {
			first = earliest(first, sess.StartTime)
		}
	}
	return first, nil
}

func earliest(a, b *time.Time) *time.Time {
	if b == nil || (a != nil && !b.Before(*a)) {
		return a
	}
	return b
}

// Copy copies the source Event's structure into a new draft (ADR 0066,
// ticket 06): its details, cover, capacity, certificate rule and Event
// template binding, days and Sessions, and, when asked, its door staff and
// Competitions with their stages. People and history (Tickets, check-ins,
// certificates, Competitors, the mail list), the gallery, files and videos,
// the forms and their short links are never copied. The dates move with the
// new start (Shift). Whoever may see the source and create Events in its
// Owner team may; a part the copier may not write, or a rule refuses, is
// left out and named in Skipped. The same id from the same source answers
// the copy made already (fresh false); an id another Event has is
// ErrConflict.
func (s *service) Copy(ctx context.Context, p authz.Principal, sourceID uuid.UUID, in CopyInput) (Copied, bool, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.ID == uuid.Nil || in.Name == "" || in.StartDate == nil {
		return Copied{}, false, ErrInvalid
	}
	source, err := s.visible(ctx, &p, sourceID)
	if err != nil {
		return Copied{}, false, err
	}
	team := source.OwnerTeam
	if !s.authz.Allow(p, resource(team), authz.Create) {
		return Copied{}, false, ErrForbidden
	}
	if made, ok, err := s.madeCopy(ctx, &p, in.ID, source.ID); ok || err != nil {
		return made, false, err
	}
	anchor, err := s.anchor(ctx, source)
	if err != nil {
		return Copied{}, false, err
	}
	shift := NewShift(anchor, *in.StartDate)
	start := *in.StartDate
	plan := CopyPlan{
		Source: source.ID,
		Event: Event{
			ID:                   in.ID,
			Name:                 in.Name,
			Description:          source.Description,
			Location:             source.Location,
			OwnerTeam:            team,
			Capacity:             source.Capacity,
			StartDate:            &start,
			EndDate:              shift.At(source.EndDate),
			Status:               StatusDraft,
			CopiedFromEventID:    &source.ID,
			Ranked:               source.Ranked,
			PrizeInfo:            source.PrizeInfo,
			RegistrationClosesAt: shift.At(source.RegistrationClosesAt),
			AttendanceRule:       source.AttendanceRule,
			AttendanceRatio:      source.AttendanceRatio,
		},
		Shift:               shift,
		By:                  lifecycle.ActorID(p.ID),
		CertificateTemplate: CopyRefused,
	}
	if s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: team}, authz.Assign) {
		plan.CertificateTemplate = CopyWrite
	}
	if in.IncludeDoorStaff {
		plan.DoorStaff = CopyRefused
		if s.authz.Allow(p, resource(team), authz.Assign) {
			plan.DoorStaff = CopyWrite
		}
	}
	if in.IncludeCompetitions {
		plan.Competitions = CopyRefused
		if s.authz.Allow(p, authz.Resource{Type: authz.TypeCompetition, OwnerTeam: team}, authz.Create) {
			plan.Competitions = CopyWrite
		}
	}
	if source.CoverImageID != nil {
		plan.Cover = CopyWrite
		err := s.checkMedia(ctx, in.ID, team, *source.CoverImageID, media.RoleEventCover)
		var refusal *media.LinkRefusal
		switch {
		case errors.As(err, &refusal):
			plan.Cover = CopyRefused
		case err != nil:
			return Copied{}, false, err
		default:
			plan.Event.CoverImageID = source.CoverImageID
		}
	}
	created, parts, err := s.store.Copy(ctx, plan)
	if errors.Is(err, ErrConflict) {
		// Another request took the id meanwhile: the same copy, or not.
		made, ok, err := s.madeCopy(ctx, &p, in.ID, source.ID)
		if ok || err != nil {
			return made, false, err
		}
		return Copied{}, false, ErrConflict
	}
	created, err = s.detail(ctx, created, err)
	if err != nil {
		return Copied{}, false, err
	}
	return Copied{Event: created, Skipped: parts}, true, nil
}

// madeCopy finds the copy a request with the id made already: ok when the
// id is a current Event copied from source that p may see, ErrConflict when
// it is any other Event, nothing when the id is free.
func (s *service) madeCopy(ctx context.Context, p *authz.Principal, id, source uuid.UUID) (Copied, bool, error) {
	existing, err := s.store.GetIncludingArchived(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Copied{}, false, nil
	}
	if err != nil {
		return Copied{}, false, err
	}
	if existing.ArchivedAt != nil || existing.CopiedFromEventID == nil || *existing.CopiedFromEventID != source || !s.VisibleTo(p, existing) {
		return Copied{}, false, ErrConflict
	}
	existing, err = s.detail(ctx, existing, nil)
	if err != nil {
		return Copied{}, false, err
	}
	return Copied{Event: existing, Skipped: []string{}}, true, nil
}
