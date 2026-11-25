package event

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
)

// Co-organizers (docs/co-organizers.md): the teams and people who manage an
// Event beside its one Owner team. The Owner team stays the Event's one
// owner; only PUT /v1/events/{id}/organizers changes the lists.

// Management is who of a Co-organizer team manages the Event: its Leaders
// alone, or every member (as the GECEKODU Owner team rule).
type Management string

const (
	ManagementLeaders Management = "leaders"
	ManagementMembers Management = "members"
)

// Valid reports whether m is a known management.
func (m Management) Valid() bool { return m == ManagementLeaders || m == ManagementMembers }

// Visibility is where a Co-organizer team is named: as Co-organizer on the
// site, the Ticket page and the certificate check, or as a helper team in
// the panel alone.
type Visibility string

const (
	VisibilityCoOrganizer Visibility = "co_organizer"
	VisibilityHelper      Visibility = "helper"
)

// Valid reports whether v is a known visibility.
func (v Visibility) Valid() bool { return v == VisibilityCoOrganizer || v == VisibilityHelper }

const (
	// MaxCoOrganizerTeams and MaxCoOrganizerPeople bound an Event's lists
	// (Microsoft Teams' co-organizer bound; Yusuf S1, 2026-11-25).
	MaxCoOrganizerTeams  = 10
	MaxCoOrganizerPeople = 10
	// CoOrganizerPersonGrace is how long after the Event a Co-organizer
	// person's rights last (Yusuf S1, 2026-11-25): derived, never stored.
	CoOrganizerPersonGrace = 30 * 24 * time.Hour
)

// CoOrganizerTeam is a Co-organizer team: its Team code and its two
// choices, both always stored.
type CoOrganizerTeam struct {
	Code       string     `json:"code"`
	Management Management `json:"management"`
	Visibility Visibility `json:"visibility"`
}

// OrganizersInput is the body of PUT /v1/events/{id}/organizers: the whole
// lists. A team's management left out is the Owner team's rule, its
// visibility left out is co_organizer.
type OrganizersInput struct {
	Teams  []OrganizerTeamInput
	People []uuid.UUID
}

// OrganizerTeamInput is one team of OrganizersInput.
type OrganizerTeamInput struct {
	Code       string
	Management *Management
	Visibility *Visibility
}

// Organizers is GET /v1/events/{id}/organizers: the Owner team and its
// rule, the Co-organizer teams and people, and whether the caller may change
// the lists.
type Organizers struct {
	OwnerTeam string `json:"ownerTeam"`
	// OwnerRule is who of the Owner team edits the Event: leaders, or
	// members for an Owner team whose rule opens editing to them.
	OwnerRule Management        `json:"ownerRule"`
	Teams     []OrganizerTeam   `json:"teams"`
	People    []OrganizerPerson `json:"people"`
	CanManage bool              `json:"canManage"`
}

// OrganizerTeam is a Co-organizer team as GET answers it, with its name as
// shown outside the panel (organizer.Name).
type OrganizerTeam struct {
	Code       string     `json:"code"`
	Name       string     `json:"name"`
	Management Management `json:"management"`
	Visibility Visibility `json:"visibility"`
}

// OrganizerPerson is a Co-organizer person as GET answers it. ExpiresAt is
// when the person's rights end (CoOrganizerPersonExpiry), left out for an
// Event with no date.
type OrganizerPerson struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Email only to who may change the list (canManage); the Event's other
	// editors see the name alone.
	Email     string     `json:"email,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// CoOrganizerPersonExpiry is when a Co-organizer person's rights on e end:
// CoOrganizerPersonGrace after the Event's end, or after its start plus a
// day when it has no end; false when it has neither.
func CoOrganizerPersonExpiry(e Event) (time.Time, bool) {
	switch {
	case e.EndDate != nil:
		return e.EndDate.Add(CoOrganizerPersonGrace), true
	case e.StartDate != nil:
		return e.StartDate.Add(24*time.Hour + CoOrganizerPersonGrace), true
	default:
		return time.Time{}, false
	}
}

// ErrCoOrganizerPersonDates refuses an edit by who edits the Event as one
// of its Co-organizer people alone that would lengthen their own rights:
// clearing the dates their end is read from, or moving it later
// (docs/co-organizers.md). The Owner team and the Co-organizer teams are
// not bound by it.
var ErrCoOrganizerPersonDates = fmt.Errorf("event: a co-organizer person may not move the end of their rights later: %w", ErrForbidden)

// checkCoOrganizerPersonDates applies ErrCoOrganizerPersonDates to p's edit
// of existing into in: only when p's right to edit rests on a Co-organizer
// person's seat (authz.ViaCoPerson), and only when existing has an end
// (CoOrganizerPersonExpiry) that in would clear or move later.
func (s *service) checkCoOrganizerPersonDates(p authz.Principal, existing, in Event) error {
	before, bounded := CoOrganizerPersonExpiry(existing)
	if !bounded {
		return nil
	}
	if s.authz.EventVia(p, s.eventResource(existing, authz.TypeEvent)) != authz.ViaCoPerson {
		return nil
	}
	if after, ok := CoOrganizerPersonExpiry(in); !ok || after.After(before) {
		return ErrCoOrganizerPersonDates
	}
	return nil
}

// PublicCoOrganizerTeams are the codes of e's Co-organizer teams anyone may
// see: those shown as Co-organizer, never a helper team; nil for none.
func PublicCoOrganizerTeams(e Event) []string {
	var out []string
	for _, team := range e.CoOrganizers {
		if team.Visibility == VisibilityCoOrganizer {
			out = append(out, team.Code)
		}
	}
	return out
}

// ErrCoOrganizersDisabled refuses a write of the Co-organizers while
// EVENT_CO_ORGANIZERS_WRITE is off (docs/co-organizers.md): until the
// decisions, the audit record and the mails ship with them, nobody changes
// the list. Reading it still works.
var ErrCoOrganizersDisabled = fmt.Errorf("event: co-organizer writes are off: %w", ErrForbidden)

// OrganizersError refuses a list of Co-organizers: the field (teams or
// people), the item's index (-1 for the list itself) and why. It is an
// ErrInvalid.
type OrganizersError struct {
	Field  string
	Index  int
	Reason string
}

// The reasons of an OrganizersError.
const (
	OrganizersTooMany           = "too_many"
	OrganizersDuplicate         = "duplicate"
	OrganizersInvalidCode       = "invalid_code"
	OrganizersOwnerTeam         = "owner_team"
	OrganizersPrivilegedTeam    = "privileged_team"
	OrganizersUnknownTeam       = "unknown_team"
	OrganizersInvalidManagement = "invalid_management"
	OrganizersInvalidVisibility = "invalid_visibility"
	OrganizersInvalidPerson     = "invalid_person"
	OrganizersInactivePerson    = "inactive_person"
)

func (e *OrganizersError) Error() string {
	return fmt.Sprintf("event: organizers: %s[%d]: %s", e.Field, e.Index, e.Reason)
}

func (e *OrganizersError) Unwrap() error { return ErrInvalid }

func teamsError(index int, reason string) error {
	return &OrganizersError{Field: "teams", Index: index, Reason: reason}
}

func peopleError(index int, reason string) error {
	return &OrganizersError{Field: "people", Index: index, Reason: reason}
}

// ownerRule is who of ownerTeam edits its Events.
func (s *service) ownerRule(ownerTeam string) Management {
	if s.policy.MembersUpdate(ownerTeam) {
		return ManagementMembers
	}
	return ManagementLeaders
}

// Organizers answers the Event's Owner team and Co-organizers to who may
// edit it; a draft p may not see is ErrNotFound.
func (s *service) Organizers(ctx context.Context, p authz.Principal, id uuid.UUID) (Organizers, error) {
	e, err := s.visible(ctx, &p, id)
	if err != nil {
		return Organizers{}, err
	}
	if !s.authz.Allow(p, s.eventResource(e, authz.TypeEvent), authz.Update) {
		return Organizers{}, ErrForbidden
	}
	people, err := s.store.CoOrganizerPeople(ctx, e.ID)
	if err != nil {
		return Organizers{}, err
	}
	return s.organizers(p, e, people), nil
}

func (s *service) organizers(p authz.Principal, e Event, people []OrganizerPerson) Organizers {
	out := Organizers{
		OwnerTeam: e.OwnerTeam,
		OwnerRule: s.ownerRule(e.OwnerTeam),
		Teams:     make([]OrganizerTeam, 0, len(e.CoOrganizers)),
		People:    make([]OrganizerPerson, 0, len(people)),
		CanManage: s.canManageOrganizers(p, e),
	}
	for _, team := range e.CoOrganizers {
		out.Teams = append(out.Teams, OrganizerTeam{
			Code: team.Code, Name: organizer.Name(team.Code),
			Management: team.Management, Visibility: team.Visibility,
		})
	}
	var expiry *time.Time
	if at, ok := CoOrganizerPersonExpiry(e); ok {
		expiry = &at
	}
	for _, person := range people {
		person.ExpiresAt = expiry
		if !out.CanManage {
			person.Email = ""
		}
		out.People = append(out.People, person)
	}
	return out
}

// canManageOrganizers reports whether p may change e's Co-organizers
// (authz.ManageOrganizers: Privileged and the Owner team's Leaders, never
// a Co-organizer): never while the writes are off
// (EVENT_CO_ORGANIZERS_WRITE).
func (s *service) canManageOrganizers(p authz.Principal, e Event) bool {
	return s.coOrganizersWrite && s.authz.Allow(p, s.eventResource(e, authz.TypeEvent), authz.ManageOrganizers)
}

// SetOrganizers replaces the Event's Co-organizer lists with in's, once
// checked (OrganizersError), and answers them as Organizers does.
func (s *service) SetOrganizers(ctx context.Context, p authz.Principal, id uuid.UUID, in OrganizersInput) (Organizers, error) {
	if !s.coOrganizersWrite {
		return Organizers{}, ErrCoOrganizersDisabled
	}
	e, err := s.visible(ctx, &p, id)
	if err != nil {
		return Organizers{}, err
	}
	if !s.canManageOrganizers(p, e) {
		// Recorded (denied) with what it asked for, when that can be read.
		var changes []organizerChange
		if teams, err := s.coOrganizerTeams(e.OwnerTeam, in.Teams); err == nil {
			if people, err := coOrganizerPeople(in.People); err == nil {
				changes = organizerChanges(e, teams, people)
			}
		}
		s.denyOrganizers(ctx, e, changes)
		return Organizers{}, ErrForbidden
	}
	teams, err := s.coOrganizerTeams(e.OwnerTeam, in.Teams)
	if err != nil {
		return Organizers{}, err
	}
	people, err := coOrganizerPeople(in.People)
	if err != nil {
		return Organizers{}, err
	}
	// Each change is a row of the Team audit record, opened before the
	// write: without its row there is no change (docs/co-organizers.md).
	changes := organizerChanges(e, teams, people)
	open, err := s.beginOrganizerAudit(ctx, p, e, changes)
	if err != nil {
		return Organizers{}, err
	}
	updated, err := s.store.SetCoOrganizers(ctx, CoOrganizersWrite{
		EventID: e.ID, OwnerTeam: e.OwnerTeam, Teams: teams, People: people, By: lifecycle.ActorID(p.ID),
		Expect: true, WasTeams: e.CoOrganizers, WasPeople: e.CoOrganizerIDs,
	})
	s.finishOrganizerAudit(ctx, open, err)
	if err != nil {
		var inactive *InactivePersonError
		if errors.As(err, &inactive) {
			return Organizers{}, peopleError(slices.Index(people, inactive.ID), OrganizersInactivePerson)
		}
		return Organizers{}, err
	}
	written, err := s.store.CoOrganizerPeople(ctx, updated.ID)
	if err != nil {
		return Organizers{}, err
	}
	return s.organizers(p, updated, written), nil
}

// coOrganizerTeams checks the teams and gives each its stored code (the
// Team's Group name) and both choices.
func (s *service) coOrganizerTeams(ownerTeam string, in []OrganizerTeamInput) ([]CoOrganizerTeam, error) {
	if len(in) > MaxCoOrganizerTeams {
		return nil, teamsError(-1, OrganizersTooMany)
	}
	out := make([]CoOrganizerTeam, 0, len(in))
	for i, team := range in {
		management := s.ownerRule(ownerTeam)
		if team.Management != nil {
			management = *team.Management
		}
		visibility := VisibilityCoOrganizer
		if team.Visibility != nil {
			visibility = *team.Visibility
		}
		if !management.Valid() {
			return nil, teamsError(i, OrganizersInvalidManagement)
		}
		if !visibility.Valid() {
			return nil, teamsError(i, OrganizersInvalidVisibility)
		}
		code := strings.TrimSpace(team.Code)
		if code == "" || strings.Contains(code, "/") {
			return nil, teamsError(i, OrganizersInvalidCode)
		}
		if s.privilegedTeam(code) {
			return nil, teamsError(i, OrganizersPrivilegedTeam)
		}
		code, ok := s.teamCode(code)
		if !ok {
			return nil, teamsError(i, OrganizersUnknownTeam)
		}
		if strings.EqualFold(code, strings.TrimSpace(ownerTeam)) {
			return nil, teamsError(i, OrganizersOwnerTeam)
		}
		if slices.ContainsFunc(out, func(t CoOrganizerTeam) bool { return strings.EqualFold(t.Code, code) }) {
			return nil, teamsError(i, OrganizersDuplicate)
		}
		out = append(out, CoOrganizerTeam{Code: code, Management: management, Visibility: visibility})
	}
	return out, nil
}

// privilegedTeam reports whether code names a Privileged root (YK, DK,
// ADMIN): they manage or read every Event already, and are never a
// Co-organizer.
func (s *service) privilegedTeam(code string) bool {
	return slices.ContainsFunc(s.policy.PrivilegedRoots, func(root string) bool {
		return strings.EqualFold(authz.RootLabel(root), code)
	})
}

// teamCode is the Team code of the one Team whose Group is named code
// (compared without case): the Group's own name. False when there is none,
// the Group is no Team (an area, a Leader group, a Privileged root), or the
// Groups cannot be read.
func (s *service) teamCode(code string) (string, bool) {
	if s.teams == nil {
		return "", false
	}
	path, ok := s.teams.TeamPath(code)
	if !ok {
		return "", false
	}
	team, role, ok := authz.TeamOf(path, s.policy.PrivilegedRoots, s.policy.LeaderSubgroups)
	if !ok || role != "" || team != path {
		return "", false
	}
	return path[strings.LastIndex(path, "/")+1:], true
}

func coOrganizerPeople(in []uuid.UUID) ([]uuid.UUID, error) {
	if len(in) > MaxCoOrganizerPeople {
		return nil, peopleError(-1, OrganizersTooMany)
	}
	out := make([]uuid.UUID, 0, len(in))
	for i, id := range in {
		if id == uuid.Nil {
			return nil, peopleError(i, OrganizersInvalidPerson)
		}
		if slices.Contains(out, id) {
			return nil, peopleError(i, OrganizersDuplicate)
		}
		out = append(out, id)
	}
	return out, nil
}

// CoOrganizersWrite is what the store's SetCoOrganizers writes: the whole
// lists of the Event, while its Owner team is still OwnerTeam. The store
// writes the difference: a row that stays keeps who added it and when, a
// team that stays has its choices updated, and By is the adder of the new
// rows alone.
type CoOrganizersWrite struct {
	EventID   uuid.UUID
	OwnerTeam string
	Teams     []CoOrganizerTeam
	People    []uuid.UUID
	By        *uuid.UUID
	// With Expect, WasTeams and WasPeople are the lists the caller read and
	// decided against (and recorded the difference of): ErrConflict when
	// the Event's lists are no longer these.
	Expect    bool
	WasTeams  []CoOrganizerTeam
	WasPeople []uuid.UUID
}

// sameCoOrganizers reports whether two lists are the same, in any order:
// the teams with their choices (codes compared without case), the people.
func sameCoOrganizers(teamsA []CoOrganizerTeam, peopleA []uuid.UUID, teamsB []CoOrganizerTeam, peopleB []uuid.UUID) bool {
	if len(teamsA) != len(teamsB) || len(peopleA) != len(peopleB) {
		return false
	}
	for _, a := range teamsA {
		if !slices.ContainsFunc(teamsB, func(b CoOrganizerTeam) bool {
			return strings.EqualFold(a.Code, b.Code) && a.Management == b.Management && a.Visibility == b.Visibility
		}) {
			return false
		}
	}
	for _, a := range peopleA {
		if !slices.Contains(peopleB, a) {
			return false
		}
	}
	return true
}

// InactivePersonError is a new Co-organizer person who is no active account
// (none, being erased, or erased): the store refuses the write. A person
// already on the list stays whatever their account became.
type InactivePersonError struct {
	ID uuid.UUID
}

func (e *InactivePersonError) Error() string { return "event: co-organizer is no active account" }

func (e *InactivePersonError) Unwrap() error { return ErrInvalid }

// withoutOwner is teams without the one ownerTeam names: an Event whose
// Owner team becomes one of its Co-organizer teams drops that row.
func withoutOwner(teams []CoOrganizerTeam, ownerTeam string) []CoOrganizerTeam {
	if !slices.ContainsFunc(teams, func(t CoOrganizerTeam) bool { return strings.EqualFold(t.Code, strings.TrimSpace(ownerTeam)) }) {
		return teams
	}
	out := make([]CoOrganizerTeam, 0, len(teams)-1)
	for _, t := range teams {
		if !strings.EqualFold(t.Code, strings.TrimSpace(ownerTeam)) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
