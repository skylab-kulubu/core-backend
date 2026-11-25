package event

import (
	"context"
	"errors"
	"log"
	"path"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// TeamGroups finds a Group by its id or path (identity.Directory): the
// Event's Owner team's, the target of a Co-organizer row of the Team audit
// record, and the Team whose Leader group lets the caller change the list.
type TeamGroups interface {
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
}

// organizerChange is one difference between an Event's Co-organizer lists
// and the ones a write gives: a team or a person added or removed, or a
// team's choices changed. It is one row of the Team audit record.
type organizerChange struct {
	action        teamaudit.Action
	before, after *teamaudit.State
}

func teamState(eventID uuid.UUID, t CoOrganizerTeam) *teamaudit.State {
	return &teamaudit.State{EventID: eventID.String(), Code: t.Code, Management: string(t.Management), Visibility: string(t.Visibility)}
}

func personState(eventID, person uuid.UUID) *teamaudit.State {
	return &teamaudit.State{EventID: eventID.String(), PersonID: person.String()}
}

// organizerChanges are the differences between e's lists and teams and
// people: the teams by code, then the people, each in its list's order.
func organizerChanges(e Event, teams []CoOrganizerTeam, people []uuid.UUID) []organizerChange {
	var out []organizerChange
	find := func(list []CoOrganizerTeam, code string) (CoOrganizerTeam, bool) {
		i := slices.IndexFunc(list, func(t CoOrganizerTeam) bool { return strings.EqualFold(t.Code, code) })
		if i < 0 {
			return CoOrganizerTeam{}, false
		}
		return list[i], true
	}
	for _, was := range e.CoOrganizers {
		now, ok := find(teams, was.Code)
		switch {
		case !ok:
			out = append(out, organizerChange{action: teamaudit.ActionCoOrganizerRemove, before: teamState(e.ID, was)})
		case now.Management != was.Management || now.Visibility != was.Visibility:
			out = append(out, organizerChange{action: teamaudit.ActionCoOrganizerChange, before: teamState(e.ID, was), after: teamState(e.ID, now)})
		}
	}
	for _, now := range teams {
		if _, ok := find(e.CoOrganizers, now.Code); !ok {
			out = append(out, organizerChange{action: teamaudit.ActionCoOrganizerAdd, after: teamState(e.ID, now)})
		}
	}
	for _, was := range e.CoOrganizerIDs {
		if !slices.Contains(people, was) {
			out = append(out, organizerChange{action: teamaudit.ActionCoOrganizerRemove, before: personState(e.ID, was)})
		}
	}
	for _, now := range people {
		if !slices.Contains(e.CoOrganizerIDs, now) {
			out = append(out, organizerChange{action: teamaudit.ActionCoOrganizerAdd, after: personState(e.ID, now)})
		}
	}
	return out
}

// organizerAudit is the open rows of one write of the list.
type organizerAudit struct {
	ids []uuid.UUID
}

// auditTarget is the Event's Owner team's Group, by its path; an Event
// without an Owner team has none (an empty Group).
func (s *service) auditTarget(ctx context.Context, e Event) (identity.Group, error) {
	owner := strings.TrimSpace(e.OwnerTeam)
	if owner == "" {
		return identity.Group{}, nil
	}
	if s.groups == nil || s.teams == nil {
		return identity.Group{}, teamaudit.ErrUnavailable
	}
	teamPath, ok := s.teams.TeamPath(owner)
	if !ok {
		return identity.Group{}, teamaudit.ErrUnavailable
	}
	g, err := s.groups.GetGroup(ctx, teamPath)
	if err != nil {
		return identity.Group{}, teamaudit.ErrUnavailable
	}
	return g, nil
}

// beginOrganizerAudit opens a pending row for each change
// (docs/co-organizers.md): the actor the request carries
// (teamaudit.WithActor), privileged or the Leader of the Event's Owner team
// or a Team above it, against the Owner team. A row that cannot be opened
// stops the write (teamaudit.ErrUnavailable): those opened already are
// finished failed. Without an audit writer nothing is recorded.
func (s *service) beginOrganizerAudit(ctx context.Context, p authz.Principal, e Event, changes []organizerChange) (*organizerAudit, error) {
	if s.audit == nil || len(changes) == 0 {
		return nil, nil
	}
	actor, ok := teamaudit.ActorFrom(ctx)
	if !ok {
		return nil, teamaudit.ErrUnavailable
	}
	target, err := s.auditTarget(ctx, e)
	if err != nil {
		return nil, err
	}
	actor.Basis, actor.ViaGroupID = teamaudit.BasisPrivileged, ""
	if !s.authz.Allow(p, resource(""), authz.ManageOrganizers) {
		via, err := s.leaderVia(ctx, p, target)
		if err != nil {
			return nil, teamaudit.ErrUnavailable
		}
		actor.Basis, actor.ViaGroupID = teamaudit.BasisLeader, via
	}
	open := &organizerAudit{}
	for _, change := range changes {
		id, err := s.audit.Begin(ctx, teamaudit.Record{
			Actor: actor, Action: change.action, GroupID: target.ID, GroupPath: target.Path,
			Before: change.before, After: change.after,
		})
		if err != nil {
			s.finishOrganizerAudit(ctx, open, err)
			return nil, err
		}
		open.ids = append(open.ids, id)
	}
	return open, nil
}

// finishOrganizerAudit writes the outcome of the rows: ok, or failed with a
// code.
func (s *service) finishOrganizerAudit(ctx context.Context, open *organizerAudit, err error) {
	if open == nil {
		return
	}
	outcome, code := teamaudit.OutcomeOK, ""
	if err != nil {
		outcome, code = teamaudit.OutcomeFailed, organizerAuditCode(err)
	}
	for _, id := range open.ids {
		if ferr := s.audit.Finish(ctx, id, outcome, code, nil); ferr != nil {
			log.Printf("events: audit record %s stays pending", id)
		}
	}
}

func organizerAuditCode(err error) string {
	var inactive *OrganizersError
	switch {
	case errors.Is(err, teamaudit.ErrUnavailable):
		return "audit_unavailable"
	case errors.As(err, &inactive):
		return "event_organizers_invalid"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	default:
		return "write_failed"
	}
}

// denyOrganizers records a write of the list authorization refused: a
// denied row for each change it asked for, or, when its lists cannot be
// read, one co_organizer.change row naming the Event alone. A row that
// cannot be written changes nothing: the write is refused either way.
func (s *service) denyOrganizers(ctx context.Context, e Event, changes []organizerChange) {
	if s.audit == nil {
		return
	}
	actor, ok := teamaudit.ActorFrom(ctx)
	if !ok {
		return
	}
	target, err := s.auditTarget(ctx, e)
	if err != nil {
		return
	}
	actor.Basis, actor.ViaGroupID = teamaudit.BasisNone, ""
	if len(changes) == 0 {
		changes = []organizerChange{{action: teamaudit.ActionCoOrganizerChange, after: &teamaudit.State{EventID: e.ID.String()}}}
	}
	for _, change := range changes {
		if err := s.audit.Deny(ctx, teamaudit.Record{
			Actor: actor, Action: change.action, GroupID: target.ID, GroupPath: target.Path,
			Before: change.before, After: change.after,
		}, "forbidden"); err != nil {
			log.Printf("events: a refused change of the Co-organizers was not recorded")
			return
		}
	}
}

// leaderVia is the id of the Team whose Leader group lets p change the list
// of an Event team owns: the team itself or one it sits under (ADR-0065).
// ErrForbidden when p leads none.
func (s *service) leaderVia(ctx context.Context, p authz.Principal, team identity.Group) (string, error) {
	if team.Path == "" {
		return "", ErrForbidden
	}
	best := ""
	for _, g := range p.Groups {
		parent, kind := path.Split(strings.TrimRight(g, "/"))
		if !slices.Contains(s.policy.LeaderSubgroups, kind) {
			continue
		}
		parent = strings.TrimRight(parent, "/")
		if parent == "" || (team.Path != parent && !strings.HasPrefix(team.Path, parent+"/")) {
			continue
		}
		if len(parent) > len(best) {
			best = parent
		}
	}
	switch {
	case best == "":
		return "", ErrForbidden
	case best == team.Path:
		return team.ID, nil
	}
	via, err := s.groups.GetGroup(ctx, best)
	if err != nil {
		return "", err
	}
	return via.ID, nil
}
