package authz

import "sort"

// Capabilities is what a caller may do, for the admin panel to decide which
// menus and buttons to show (ADR-0059: the panel does not read the token).
// Every answer is an Allow or Permitted decision of the authorizer that
// decides the caller's real requests; nothing here is a second copy of a
// rule. Contract: docs/authz-roles.md.
type Capabilities struct {
	// Permissions are the roles of the contract (PermissionRoles) the
	// caller holds as the role mode reads them, in the contract's order. In
	// the groups mode a Privileged member holds them all.
	Permissions []string `json:"permissions"`
	// Can are the decisions that name no Owner team, every key of
	// AppAbilities present.
	Can map[string]bool `json:"can"`
	// Teams are the Owner teams the caller is a member or Leader of, by
	// name, with the decisions for a record of that team.
	Teams []TeamCapabilities `json:"teams"`
	// OtherTeams are the decisions for a record of an Owner team the caller
	// is in no Group of.
	OtherTeams ScopeCapabilities `json:"otherTeams"`
	// NoOwnerTeam are the decisions for a record without an Owner team.
	NoOwnerTeam ScopeCapabilities `json:"noOwnerTeam"`
}

// TeamCapabilities are the decisions for a record of one Owner team.
type TeamCapabilities struct {
	Team string `json:"team"`
	// Levels are the caller's levels in the team: LEADER, MEMBER or both.
	Levels []Level         `json:"levels"`
	Can    map[string]bool `json:"can"`
}

// ScopeCapabilities are team decisions with every key of TeamAbilities.
type ScopeCapabilities struct {
	Can map[string]bool `json:"can"`
}

// ability is one decision: an action on a resource.
type ability struct {
	key    string
	r      Resource
	action Action
}

// teamAbilities are decided for a record of an Owner team (Resource's
// OwnerTeam set per scope). Door check-in is the Owner team's answer
// without the Event's door staff and with Team door scan off.
var teamAbilities = []ability{
	{"event.create", Resource{Type: TypeEvent}, Create},
	{"event.update", Resource{Type: TypeEvent}, Update},
	{"event.delete", Resource{Type: TypeEvent}, Delete},
	{"event.assignDoorStaff", Resource{Type: TypeEvent}, Assign},
	{"ticket.read", Resource{Type: TypeTicket}, Read},
	{"ticket.assign", Resource{Type: TypeTicket}, Assign},
	{"door.checkIn", Resource{Type: TypeTicket}, Validate},
	{"competitor.read", Resource{Type: TypeCompetitor}, Read},
	{"competitor.create", Resource{Type: TypeCompetitor}, Create},
	{"competitor.update", Resource{Type: TypeCompetitor}, Update},
	{"competitor.delete", Resource{Type: TypeCompetitor}, Delete},
	{"certificate.read", Resource{Type: TypeCertificate}, Read},
	{"certificate.issue", Resource{Type: TypeCertificate}, Issue},
	{"certificate.revoke", Resource{Type: TypeCertificate}, Revoke},
	{"certificateTemplate.read", Resource{Type: TypeCertificateTemplate}, Read},
	{"certificateTemplate.create", Resource{Type: TypeCertificateTemplate}, Create},
	{"certificateTemplate.update", Resource{Type: TypeCertificateTemplate}, Update},
	{"certificateTemplate.assign", Resource{Type: TypeCertificateTemplate}, Assign},
}

// appAbilities name no Owner team.
var appAbilities = []ability{
	{"season.write", Resource{Type: TypeSeason}, Create},
	{"group.read", Resource{Type: TypeGroup}, Read},
	{"group.write", Resource{Type: TypeGroup}, Update},
	{"user.read", Resource{Type: TypeUser}, Read},
	{"user.write", Resource{Type: TypeUser}, Update},
	{"user.create", Resource{Type: TypeUser}, Create},
	{"user.delete", Resource{Type: TypeUser}, Delete},
	{"media.list", Resource{Type: TypeMedia}, List},
	{"media.delete", Resource{Type: TypeMedia}, Delete},
	{"media.readPrivate", Resource{Type: TypeMediaReadLink}, Read},
	{"media.uploadEventMedia", Resource{Type: TypeMedia, MediaUploader: MediaUploaderEventEditor}, Upload},
	{"media.uploadCertificateTemplateMedia", Resource{Type: TypeMedia, MediaUploader: MediaUploaderCertificateTemplateEditor}, Upload},
	{"url.create", Resource{Type: TypeURL}, Create},
	{"url.listMine", Resource{Type: TypeURL}, ReadMe},
	{"url.moderate", Resource{Type: TypeURL}, Read},
	{"formLink.read", Resource{Type: TypeFormLink}, Read},
	{"formLink.write", Resource{Type: TypeFormLink}, Update},
	{"githubActivity.read", Resource{Type: TypeGithubActivity}, Read},
}

// TeamAbilities are the keys of every team scope's Can, in order.
func TeamAbilities() []string { return abilityKeys(teamAbilities) }

// AppAbilities are the keys of Capabilities.Can, in order.
func AppAbilities() []string { return abilityKeys(appAbilities) }

func abilityKeys(list []ability) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.key
	}
	return out
}

// otherTeam is an Owner team name no Group path can hold: a path segment
// never contains a NUL.
const otherTeam = "\x00other"

func (a *authorizer) Capabilities(p Principal) Capabilities {
	out := Capabilities{
		Permissions: []string{},
		Can:         map[string]bool{},
		Teams:       []TeamCapabilities{},
		OtherTeams:  ScopeCapabilities{Can: a.teamDecisions(p, otherTeam)},
		NoOwnerTeam: ScopeCapabilities{Can: a.teamDecisions(p, "")},
	}
	for _, role := range PermissionRoles() {
		if a.Permitted(p, role) {
			out.Permissions = append(out.Permissions, role)
		}
	}
	for _, ab := range appAbilities {
		out.Can[ab.key] = a.Allow(p, ab.r, ab.action)
	}
	teams := a.ownerTeamCandidates(p)
	sort.Strings(teams)
	for _, team := range teams {
		out.Teams = append(out.Teams, TeamCapabilities{
			Team: team, Levels: a.ownerLevels(p, team), Can: a.teamDecisions(p, team),
		})
	}
	return out
}

func (a *authorizer) teamDecisions(p Principal, team string) map[string]bool {
	out := make(map[string]bool, len(teamAbilities))
	for _, ab := range teamAbilities {
		r := ab.r
		r.OwnerTeam = team
		out[ab.key] = a.Allow(p, r, ab.action)
	}
	return out
}
