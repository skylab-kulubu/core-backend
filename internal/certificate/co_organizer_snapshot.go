package certificate

import (
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
)

// coOrganizerSnapshot is what a certificate issued now keeps of its Event's
// Co-organizers (docs/co-organizers.md): the teams shown outside the panel
// at that moment, never a helper team or a person. Never nil: an empty list
// is "none", told apart from a certificate issued before core kept it (nil,
// NULL).
func coOrganizerSnapshot(ev event.Event) []string {
	teams := event.PublicCoOrganizerTeams(ev)
	if teams == nil {
		return []string{}
	}
	return teams
}

// coOrganizerTeamOf reports whether team is one of ev's Co-organizer teams
// shown outside the panel (co_organizer): a helper team's template is never
// bound to the Event (its name would show on the certificate's design).
func coOrganizerTeamOf(ev event.Event, team string) bool {
	for _, co := range event.PublicCoOrganizerTeams(ev) {
		if normalizeTeam(co) == normalizeTeam(team) {
			return true
		}
	}
	return false
}

// coOrganizerNames names a certificate's snapshot as its verification shows
// it (organizer.CoNames); empty for none and for an old certificate.
func coOrganizerNames(c Certificate) string {
	return organizer.CoNames(c.CoOrganizerTeams)
}
