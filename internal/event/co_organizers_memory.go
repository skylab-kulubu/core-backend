package event

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// SetCoOrganizers replaces the Event's Co-organizers as the database does,
// in the order it reads them back (teams by code, people by id). The memory
// store holds no accounts: every person is taken.
func (s *MemoryStore) SetCoOrganizers(_ context.Context, w CoOrganizersWrite) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[w.EventID]
	if !ok || e.ArchivedAt != nil {
		return Event{}, ErrNotFound
	}
	if e.OwnerTeam != w.OwnerTeam {
		return Event{}, ErrConflict
	}
	e.CoOrganizers, e.CoOrganizerIDs = sortedCoOrganizers(w.Teams, w.People)
	s.byID[e.ID] = e
	return s.detailed(e), nil
}

// CoOrganizerPeople answers the Event's Co-organizer people by id alone:
// the memory store holds no names.
func (s *MemoryStore) CoOrganizerPeople(_ context.Context, eventID uuid.UUID) ([]OrganizerPerson, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]OrganizerPerson, 0, len(e.CoOrganizerIDs))
	for _, id := range e.CoOrganizerIDs {
		out = append(out, OrganizerPerson{ID: id})
	}
	return out, nil
}

func sortedCoOrganizers(teams []CoOrganizerTeam, people []uuid.UUID) ([]CoOrganizerTeam, []uuid.UUID) {
	var outTeams []CoOrganizerTeam
	if len(teams) > 0 {
		outTeams = slices.Clone(teams)
		slices.SortFunc(outTeams, func(a, b CoOrganizerTeam) int { return strings.Compare(a.Code, b.Code) })
	}
	var outPeople []uuid.UUID
	if len(people) > 0 {
		outPeople = slices.Clone(people)
		slices.SortFunc(outPeople, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	}
	return outTeams, outPeople
}

// HasCoOrganizerSeat is the memory store's: a current Event names p as a
// person whose rights have not ended, or a team whose code is a segment
// of one of p's Groups.
func (s *MemoryStore) HasCoOrganizerSeat(p authz.Principal) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, e := range s.byID {
		if e.ArchivedAt != nil {
			continue
		}
		if r := AuthzResource(e, authz.TypeEvent, now); slices.Contains(r.CoOrganizerIDs, p.ID) {
			return true
		}
		for _, team := range e.CoOrganizers {
			for _, group := range p.Groups {
				if slices.Contains(strings.Split(strings.Trim(group, "/"), "/"), team.Code) {
					return true
				}
			}
		}
	}
	return false
}
