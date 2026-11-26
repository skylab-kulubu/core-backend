package event

import (
	"context"
	"errors"
	"slices"
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
	SkippedAttendeeNote        = "attendeeNote"
	// SkippedCoOrganizerPeople: the copier may not change the copy's
	// Co-organizers (docs/co-organizers.md).
	SkippedCoOrganizerPeople = "coOrganizerPeople"
	// SkippedTicketImage: the source's own ticket image (its "Ayrı
	// görsel") may not be used in the copy's Owner team, as a cover
	// (docs/ticket-image.md, docs/event-copy.md).
	SkippedTicketImage = "ticketImage"
)

// skippedOrder is the order the answer lists skipped parts in.
var skippedOrder = []string{SkippedCertificateTemplate, SkippedDoorStaff, SkippedCover, SkippedCompetitions, SkippedAttendeeNote, SkippedCoOrganizerPeople, SkippedTicketImage}

// CopyInput is what POST /v1/events/{id}/copy asks for (ADR 0066, ticket
// 06): the copy's id (the client makes it, so a retried request finds the
// copy it made), its name and its new start.
type CopyInput struct {
	ID                  uuid.UUID
	Name                string
	StartDate           *time.Time
	IncludeDoorStaff    bool
	IncludeCompetitions bool
	// IncludeExternalRegistration copies the source's external
	// registration link; IncludeAttendeeNote its attendee note. The panel
	// asks for each; a client that sends neither copies neither.
	IncludeExternalRegistration bool
	IncludeAttendeeNote         bool
	// OwnerTeam is the copy's Owner team (ticket 16); empty is the
	// source's.
	OwnerTeam string
	// IncludeCoOrganizerPeople copies the source's Co-organizer people
	// whose accounts are active, for who may change the copy's
	// Co-organizers (docs/co-organizers.md).
	IncludeCoOrganizerPeople bool
	// SkipCertificateTemplate leaves the source's certificate template out:
	// its Event template and its binding (named skipped when it has one).
	// The default copies them.
	SkipCertificateTemplate bool
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
	By        *uuid.UUID
	DoorStaff CopyPart
	Cover     CopyPart
	// CertificateTemplate is the copy's binding to the source's team or
	// club template: CopyWrite for who may bind in the copy's Owner team.
	CertificateTemplate CopyPart
	// EventTemplate is the source's Event template, copied as the copy's
	// own (docs/certificate-event-templates.md): CopyWrite unless the
	// copier asked to leave it out, since who creates the copy may make
	// its Event template. EventTemplateSourceLinks copies the design's
	// reference and edit address too, for who may read the source
	// template.
	EventTemplate            CopyPart
	EventTemplateSourceLinks bool
	Competitions             CopyPart
	// AttendeeNote is CopyWrite when Event carries the source's note,
	// CopyRefused when the copier may not read it (the answer names it
	// skipped).
	AttendeeNote CopyPart
	// CoOrganizerTeams are the copy's Co-organizer teams: the source's,
	// with their choices, but the copy's Owner team (docs/co-organizers.md).
	CoOrganizerTeams []CoOrganizerTeam
	// CoOrganizerPeople copies the source's Co-organizer people whose
	// accounts are active (CopyWrite); CopyRefused names them skipped when
	// the source has some.
	CoOrganizerPeople CopyPart
	// TicketImage is what the copy does with the source's own ticket image
	// (an "Ayrı görsel"): CopyWrite copies it; CopyRefused (the Team media
	// library refuses it in the copy's Owner team) and CopyNot (nobody
	// checked it) leave it out and name it skipped. A cover region comes
	// along whenever the copy has the same cover.
	TicketImage CopyPart
}

// ticketImageSource reads the Media of an Event's own ticket image (its
// "Ayrı görsel", docs/ticket-image.md), nil for none: the Postgres store.
// A store without it copies no ticket image.
type ticketImageSource interface {
	TicketImageUpload(ctx context.Context, eventID uuid.UUID) (*uuid.UUID, error)
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
// ticket 06): its details, cover, capacity, certificate rule, Event
// template (an own copy) or template binding, days and Sessions, and, when asked, its door staff,
// Competitions with their stages, external registration link and attendee
// note. People and history (Tickets, check-ins,
// certificates, Competitors, the mail list), the gallery, files and videos,
// the forms and their short links are never copied. The dates move with the
// new start (Shift). Whoever may see the source and create Events in its
// Owner team may; into another Owner team (ticket 16), whoever may edit the
// source and create Events in that team, and each part is decided for that
// team. A part the copier may not write, or a rule refuses, is left out and
// named in Skipped. Its Co-organizer teams come along with their choices
// (while EVENT_CO_ORGANIZERS_WRITE is on), and, when asked, its
// Co-organizer people whose accounts are active, for who may change the
// copy's Co-organizers (docs/co-organizers.md). A Co-organizer of the
// source who may not create Events in its Owner team copies it into that
// team, and only while they still manage the copy. The same id from the same source into the same team
// answers the copy made already (fresh false); an id another Event has, or
// that copy in another team, is ErrConflict.
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
	if target := strings.TrimSpace(in.OwnerTeam); target != "" && target != team {
		// Into another team (ticket 16): who may edit the source, not who
		// only reads it, and may create Events in the target.
		if !s.authz.Allow(p, resource(team), authz.Update) {
			return Copied{}, false, ErrForbidden
		}
		team = target
	}
	// A Co-organizer of the source who may not create Events in its Owner
	// team copies it into that team, never into their own
	// (docs/co-organizers.md): checked below, once the copy's
	// Co-organizers are known, that they still manage the copy.
	byCoOrganizer := false
	if !s.authz.Allow(p, resource(team), authz.Create) {
		via := s.authz.EventVia(p, s.eventResource(source, authz.TypeEvent))
		if team != source.OwnerTeam || (via != authz.ViaCoTeam && via != authz.ViaCoPerson) {
			return Copied{}, false, ErrForbidden
		}
		byCoOrganizer = true
	}
	if made, ok, err := s.madeCopy(ctx, &p, in.ID, source.ID, team); ok || err != nil {
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
			ID:              in.ID,
			Name:            in.Name,
			Description:     source.Description,
			Location:        source.Location,
			LocationLat:     source.LocationLat,
			LocationLng:     source.LocationLng,
			LocationAddress: source.LocationAddress,
			OwnerTeam:       team,
			Capacity:        source.Capacity,
			WaitlistEnabled: source.WaitlistEnabled,
			// The door's mail switches are the organizer's choices: the
			// copy keeps them (docs/attendance-mail.md).
			AttendanceWelcomeMail:  source.AttendanceWelcomeMail,
			AttendanceRecordedMail: source.AttendanceRecordedMail,
			// "Otomatik bilet gönder" is how the Event is run: the copy keeps
			// it (docs/form-respondents.md).
			FormResponsesAutoTicket: source.FormResponsesAutoTicket,
			// The door's mode is how the Event is run: the copy keeps it
			// (CONTEXT: Entry mode).
			EntryMode:            source.EntryMode,
			StartDate:            &start,
			EndDate:              shift.At(source.EndDate),
			Status:               StatusDraft,
			CopiedFromEventID:    &source.ID,
			Ranked:               source.Ranked,
			PrizeInfo:            source.PrizeInfo,
			RegistrationClosesAt: shift.At(source.RegistrationClosesAt),
			AttendanceRule:       source.AttendanceRule,
			AttendanceRatio:      source.AttendanceRatio,
			AttendanceMinCount:   source.AttendanceMinCount,
			// A copy never sends its certificates by itself, whatever the
			// source had: the organizer turns it on (docs/certificate-rule.md).
			AttendanceAutoFinalize: false,
			// The feedback mode and whether the form is mailed when the
			// Event ends come along (the organizer's choices); the feedback
			// form does not (a form is in one place only; etkinlik geri
			// bildirim formu §2.1).
			FeedbackMode:     source.FeedbackMode,
			FeedbackAutoSend: source.FeedbackAutoSend,
			// The theme and the ticket colour are the Event's look: the copy
			// keeps them (docs/event-theme.md).
			Theme:             source.Theme,
			TicketAccentColor: source.TicketAccentColor,
		},
		Shift:               shift,
		By:                  lifecycle.ActorID(p.ID),
		CertificateTemplate: CopyRefused,
		EventTemplate:       CopyRefused,
	}
	// The Co-organizer teams come along while the list may be written
	// (EVENT_CO_ORGANIZERS_WRITE: no list changes without its record),
	// but the one that is the copy's Owner team.
	if s.coOrganizersWrite {
		plan.CoOrganizerTeams = withoutOwner(slices.Clone(source.CoOrganizers), team)
	}
	// Each part is decided on the copy as it will be: its Owner team and
	// the Co-organizer teams it gets.
	planned := plan.Event
	planned.CoOrganizers = plan.CoOrganizerTeams
	if in.IncludeCoOrganizerPeople {
		plan.CoOrganizerPeople = CopyRefused
		if s.coOrganizersWrite && s.authz.Allow(p, s.eventResource(planned, authz.TypeEvent), authz.ManageOrganizers) {
			plan.CoOrganizerPeople = CopyWrite
			planned.CoOrganizerIDs = slices.Clone(source.CoOrganizerIDs)
		}
	}
	if byCoOrganizer && !s.authz.Allow(p, s.eventResource(planned, authz.TypeEvent), authz.Update) {
		return Copied{}, false, ErrForbidden
	}
	if !in.SkipCertificateTemplate {
		plan.EventTemplate = CopyWrite
		plan.EventTemplateSourceLinks = s.authz.Allow(p, AuthzResource(source, authz.TypeCertificateTemplate, time.Now()), authz.Read)
		if s.authz.Allow(p, s.eventResource(planned, authz.TypeCertificateTemplate), authz.Assign) {
			plan.CertificateTemplate = CopyWrite
		}
	}
	if in.IncludeDoorStaff {
		plan.DoorStaff = CopyRefused
		if s.authz.Allow(p, resource(team), authz.Assign) {
			plan.DoorStaff = CopyWrite
		}
	}
	if in.IncludeCompetitions {
		plan.Competitions = CopyRefused
		if s.authz.Allow(p, s.eventResource(planned, authz.TypeCompetition), authz.Create) {
			plan.Competitions = CopyWrite
		}
	}
	if in.IncludeExternalRegistration {
		plan.Event.ExternalRegistrationURL = source.ExternalRegistrationURL
		if plan.Event, err = normalizeExternalRegistration(plan.Event); err != nil {
			return Copied{}, false, err
		}
	}
	if in.IncludeAttendeeNote && source.AttendeeNote != nil && *source.AttendeeNote != "" {
		// The note (the venue's Wi-Fi) comes along only for who may read
		// it on the source.
		plan.AttendeeNote = CopyRefused
		if s.readsAttendeeNote(p, source) {
			plan.AttendeeNote = CopyWrite
			plan.Event.AttendeeNote = source.AttendeeNote
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
	// The source's own ticket image comes along when the copy's Owner team
	// may use it, as the cover (docs/event-copy.md); a store that cannot
	// tell which image it is copies none of its own (CopyNot).
	if images, ok := s.store.(ticketImageSource); ok {
		upload, err := images.TicketImageUpload(ctx, source.ID)
		if err != nil {
			return Copied{}, false, err
		}
		plan.TicketImage = CopyWrite
		if upload != nil {
			err := s.checkMedia(ctx, in.ID, team, *upload, media.RoleEventTicketImage)
			var refusal *media.LinkRefusal
			switch {
			case errors.As(err, &refusal):
				plan.TicketImage = CopyRefused
			case err != nil:
				return Copied{}, false, err
			}
		}
	}
	// What the copy carries of the Co-organizers is recorded, each a
	// co_organizer.add row (reason copy), opened before the write.
	var carried []uuid.UUID
	if plan.CoOrganizerPeople == CopyWrite {
		carried = source.CoOrganizerIDs
	}
	open, byPerson, err := s.copyOrganizerAudit(ctx, p, plan.Event, plan.CoOrganizerTeams, carried)
	if err != nil {
		return Copied{}, false, err
	}
	created, parts, err := s.store.Copy(ctx, plan)
	s.finishCopyAudit(ctx, open, byPerson, created, err)
	if errors.Is(err, ErrConflict) {
		// Another request took the id meanwhile: the same copy, or not.
		made, ok, err := s.madeCopy(ctx, &p, in.ID, source.ID, team)
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
// id is a current Event of team copied from source that p may see,
// ErrConflict when it is any other Event, nothing when the id is free.
func (s *service) madeCopy(ctx context.Context, p *authz.Principal, id, source uuid.UUID, team string) (Copied, bool, error) {
	existing, err := s.store.GetIncludingArchived(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Copied{}, false, nil
	}
	if err != nil {
		return Copied{}, false, err
	}
	if existing.ArchivedAt != nil || existing.CopiedFromEventID == nil || *existing.CopiedFromEventID != source ||
		existing.OwnerTeam != team || !s.VisibleTo(p, existing) {
		return Copied{}, false, ErrConflict
	}
	existing, err = s.detail(ctx, existing, nil)
	if err != nil {
		return Copied{}, false, err
	}
	return Copied{Event: existing, Skipped: []string{}}, true, nil
}
