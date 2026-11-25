package authz

import (
	"slices"
	"strings"
)

// Co-organizers (docs/co-organizers.md, ADR 0092): an Event's organizer
// teams are its Owner team and its Co-organizer teams; its Co-organizer
// people stand beside them. A person may do a job on the Event when, in any
// organizer team, they meet the level that team asks for it, or are a
// Co-organizer person (a Leader's level). What stays the Owner team's
// (deleting and archiving the Event, changing its Owner team, its
// Co-organizer list, assigning its door staff) reads the Owner team alone.

// ManageOrganizers is changing an Event's Co-organizer list: Privileged
// (event:manage) and the Owner team's Leaders (a Leader of a Team above it
// included); on an Event without an Owner team, Privileged alone.
const ManageOrganizers Action = "MANAGE_ORGANIZERS"

// CoTeam is an Event's Co-organizer team as a decision reads it.
type CoTeam struct {
	Code string
	// MembersManage is the team's management "members": its members who
	// are no Leaders manage the Event as the GECEKODU Owner team's do
	// (what takes Update opens to them, what takes a Leader does not).
	// False ("leaders"): being its member gives nothing, not the
	// certificate roles, not Team door scan, not team_competition_manage.
	MembersManage bool
}

// Via is what p's rights on an Event rest on, the strongest one: Privileged
// (event:manage), the Owner team, a Co-organizer team, a Co-organizer
// person, or none.
type Via string

const (
	ViaPrivileged Via = "privileged"
	ViaOwner      Via = "owner"
	ViaCoTeam     Via = "coTeam"
	ViaCoPerson   Via = "coPerson"
	ViaNone       Via = "none"
)

// seat is p's standing in one organizer team of an Event, or as one of its
// Co-organizer people: a Leader, and a member whose membership counts (in
// the Owner team; in a Co-organizer team whose management is members).
type seat struct {
	team          string
	owner, person bool
	leader        bool
	member        bool
	membersManage bool
}

// seats are p's seats on r's Event: none for a team p is not in, and none
// for a "leaders" Co-organizer team p is only a member of.
func (a *authorizer) seats(p Principal, r Resource) []seat {
	var out []seat
	if levels := a.ownerLevels(p, r.OwnerTeam); len(levels) > 0 {
		out = append(out, seat{
			team: r.OwnerTeam, owner: true,
			leader: slices.Contains(levels, LevelLeader), member: slices.Contains(levels, LevelMember),
		})
	}
	for _, co := range r.CoOwnerTeams {
		code := strings.TrimSpace(co.Code)
		if code == "" || strings.EqualFold(code, strings.TrimSpace(r.OwnerTeam)) {
			continue
		}
		levels := a.ownerLevels(p, code)
		s := seat{
			team: code, membersManage: co.MembersManage,
			leader: slices.Contains(levels, LevelLeader),
			member: co.MembersManage && slices.Contains(levels, LevelMember),
		}
		if s.leader || s.member {
			out = append(out, s)
		}
	}
	if p.ID != "" && slices.Contains(r.CoOrganizerIDs, p.ID) {
		out = append(out, seat{person: true, leader: true})
	}
	return out
}

// seatLevels are the levels seat s must hold for action on r's Event: the
// Owner team's row of the Event permission table for the Owner team, the
// GECEKODU row's pattern for a "members" Co-organizer team, a Leader
// otherwise.
func (a *authorizer) seatLevels(s seat, r Resource, action Action) []Level {
	switch {
	case s.owner:
		return a.requiredLevels(r.OwnerTeam, action)
	case s.membersManage && action != Delete:
		return []Level{LevelLeader, LevelMember}
	default:
		return []Level{LevelLeader}
	}
}

func (s seat) holds(needed []Level) bool {
	return (s.leader && slices.Contains(needed, LevelLeader)) || (s.member && slices.Contains(needed, LevelMember))
}

// organizerAllows reports whether a seat of p on r's Event holds the levels
// action takes; ownerOnly reads the Owner team's seat alone.
func (a *authorizer) organizerAllows(p Principal, r Resource, action Action, ownerOnly bool) bool {
	for _, s := range a.seats(p, r) {
		if ownerOnly && !s.owner {
			continue
		}
		if s.holds(a.seatLevels(s, r, action)) {
			return true
		}
	}
	return false
}

// anyLeaderSeat reports whether p is a Leader of one of r's Event's
// organizer teams or one of its Co-organizer people.
func (a *authorizer) anyLeaderSeat(p Principal, r Resource) bool {
	return slices.ContainsFunc(a.seats(p, r), func(s seat) bool { return s.leader })
}

// ownerLeader reports whether p is a Leader of r's Owner team (a Leader of
// a Team above it included).
func (a *authorizer) ownerLeader(p Principal, r Resource) bool {
	return r.OwnerTeam != "" && slices.Contains(a.ownerLevels(p, r.OwnerTeam), LevelLeader)
}

// EventVia is what p's right to edit r's Event rests on, the strongest
// one: ViaPrivileged, ViaOwner, ViaCoTeam, ViaCoPerson, or ViaNone when p
// may not edit it.
func (a *authorizer) EventVia(p Principal, r Resource) Via {
	if a.privileged(p, RoleEventManage) {
		return ViaPrivileged
	}
	via := ViaNone
	for _, s := range a.seats(p, r) {
		if !s.holds(a.seatLevels(s, r, Update)) {
			continue
		}
		switch {
		case s.owner:
			return ViaOwner
		case !s.person:
			via = ViaCoTeam
		case via == ViaNone:
			via = ViaCoPerson
		}
	}
	return via
}

// CoOrganizerSeats tells whether p holds a seat on some current Event as one
// of its Co-organizers: a person whose rights have not ended, or a member
// of a Co-organizer team. The Media upload rules of an Event's editor
// (MediaUploaderEventEditor, MediaUploaderCertificateTemplateEditor) read
// it, as they name no Event; which Event the Media ends up on is decided
// when it is linked, so the answer may be wide (a "leaders" team's member).
type CoOrganizerSeats interface {
	HasCoOrganizerSeat(p Principal) bool
}

// WithCoOrganizerSeats lets the Media upload rules of an Event's editor
// count Co-organizers. Without it only the Owner teams' editors upload.
func WithCoOrganizerSeats(s CoOrganizerSeats) Option {
	return func(a *authorizer) { a.coSeats = s }
}

func (a *authorizer) hasCoOrganizerSeat(p Principal) bool {
	return a.coSeats != nil && p.ID != "" && !p.ServiceAccount && a.coSeats.HasCoOrganizerSeat(p)
}
