package event

import (
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// AuthzResource is the one place a decision on an Event is built from the
// Event (docs/co-organizers.md): its Owner team, its Co-organizer teams with
// their management, its Co-organizer people whose rights have not ended at
// now (CoOrganizerPersonExpiry; an Event with no date ends none), and its
// door staff. A decision of type t (the Event, its days, Sessions, Tickets,
// certificates, Competitions) on e reads this; an architecture test keeps
// any other package from building one out of an Event's Owner team.
func AuthzResource(e Event, t authz.Type, now time.Time) authz.Resource {
	r := authz.Resource{Type: t, OwnerTeam: e.OwnerTeam}
	for _, id := range e.DoorStaffIDs {
		r.DoorStaffIDs = append(r.DoorStaffIDs, id.String())
	}
	for _, team := range e.CoOrganizers {
		r.CoOwnerTeams = append(r.CoOwnerTeams, authz.CoTeam{Code: team.Code, MembersManage: team.Management == ManagementMembers})
	}
	if len(e.CoOrganizerIDs) > 0 {
		if end, ok := CoOrganizerPersonExpiry(e); !ok || now.Before(end) {
			for _, id := range e.CoOrganizerIDs {
				r.CoOrganizerIDs = append(r.CoOrganizerIDs, id.String())
			}
		}
	}
	return r
}

// Viewer is what the signed-in caller may do on an Event, decided on the
// Event itself, its Co-organizers included: the panel reads it instead of
// its Owner team's capabilities (docs/co-organizers.md). Via is what the
// right to edit rests on.
type Viewer struct {
	Via authz.Via       `json:"via"`
	Can map[string]bool `json:"can"`
}

// viewerAbilities are the keys of Viewer.Can and the decision each is.
// event.manageOrganizers is the one that also reads EVENT_CO_ORGANIZERS_WRITE
// (canManageOrganizers).
var viewerAbilities = []struct {
	key    string
	typ    authz.Type
	action authz.Action
}{
	{"event.update", authz.TypeEvent, authz.Update},
	{"event.delete", authz.TypeEvent, authz.Delete},
	{"event.publish", authz.TypeEvent, authz.Update},
	{"event.manageOrganizers", authz.TypeEvent, authz.ManageOrganizers},
	{"event.assignDoorStaff", authz.TypeEvent, authz.Assign},
	{"ticket.read", authz.TypeTicket, authz.Read},
	{"ticket.assign", authz.TypeTicket, authz.Assign},
	{"certificate.issue", authz.TypeCertificate, authz.Issue},
	{"certificate.revoke", authz.TypeCertificate, authz.Revoke},
	{"certificate.template.assign", authz.TypeCertificateTemplate, authz.Assign},
	{"competition.manage", authz.TypeCompetition, authz.Update},
}

// ViewerAbilities are the keys of Viewer.Can, in order.
func ViewerAbilities() []string {
	out := make([]string, len(viewerAbilities))
	for i, a := range viewerAbilities {
		out[i] = a.key
	}
	return out
}

// viewer is what p may do on e.
// viewer is what p may do on e: each key the decision its endpoint takes.
// Three read more than one decision: event.manageOrganizers also
// EVENT_CO_ORGANIZERS_WRITE; event.delete also that only event:manage
// archives a published or cancelled Event (Delete, ADR 0066 decision 3);
// certificate.template.assign also that who may edit the Event binds its
// template (docs/certificate-event-templates.md).
func (s *service) viewer(p authz.Principal, e Event) *Viewer {
	v := &Viewer{Via: s.authz.EventVia(p, s.eventResource(e, authz.TypeEvent)), Can: make(map[string]bool, len(viewerAbilities))}
	for _, a := range viewerAbilities {
		v.Can[a.key] = s.authz.Allow(p, s.eventResource(e, a.typ), a.action)
	}
	v.Can["event.manageOrganizers"] = s.canManageOrganizers(p, e)
	v.Can["event.delete"] = v.Can["event.delete"] && (e.Status == StatusDraft || s.authz.Permitted(p, authz.RoleEventManage))
	v.Can["certificate.template.assign"] = v.Can["certificate.template.assign"] || v.Can["event.update"]
	return v
}
