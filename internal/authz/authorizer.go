package authz

import (
	"slices"
	"strings"
)

type Authorizer interface {
	Allow(p Principal, r Resource, a Action) bool
	// Permitted reports whether p holds role, one of PermissionRoles, the
	// way the role mode reads it: the role mode's answer to every
	// Privileged check that role stands for.
	Permitted(p Principal, role string) bool
	// Capabilities is what p may do, every answer an Allow or Permitted
	// decision (GET /v1/users/me/capabilities).
	Capabilities(p Principal) Capabilities
}

type authorizer struct {
	policy  Policy
	mode    RoleMode
	metrics *RoleMetrics
}

// Option configures an Authorizer.
type Option func(*authorizer)

// WithRoleMetrics counts the both mode's disagreements into m.
func WithRoleMetrics(m *RoleMetrics) Option {
	return func(a *authorizer) { a.metrics = m }
}

// NewAuthorizer decides by policy. A RoleMode it does not know decides as
// the groups mode; ParseRoleMode keeps typos out of it.
func NewAuthorizer(policy Policy, options ...Option) Authorizer {
	a := &authorizer{policy: policy, mode: policy.RoleMode}
	switch a.mode {
	case RoleModeBoth, RoleModeRoles:
	default:
		a.mode = RoleModeGroups
	}
	for _, option := range options {
		option(a)
	}
	a.metrics.setMode(a.mode)
	return a
}

func (a *authorizer) Allow(p Principal, r Resource, action Action) bool {
	switch r.Type {
	case TypeEvent, TypeEventDay, TypeSession:
		return a.allowEvent(p, r, action)
	case TypeSeason:
		if action == Read {
			return true
		}
		return a.privileged(p, RoleSeasonManage)
	case TypeTicket:
		return a.allowTicket(p, r, action)
	case TypeCompetitor:
		return a.allowCompetitor(p, r, action)
	case TypeMedia:
		return a.allowMedia(p, r, action)
	case TypeMediaAttachment:
		// Only a product's own service identity manages the Media
		// attachments of its records; a person never does, whatever roles
		// they hold. Which product it is, and that the records are its own,
		// the media service checks.
		return (action == Create || action == Delete) && isServiceProduct(p.Product) && hasRole(p, "media:attach")
	case TypeMediaReadLink:
		switch action {
		case Create:
			// The product that manages a private Media's attachments decides
			// who may open it (Skyforms for an Answer file) and asks for the
			// link with the same service identity and role. Which product
			// owns the Media, the media service checks.
			return isServiceProduct(p.Product) && hasRole(p, "media:attach")
		case Read:
			// A privileged admin opens core's own private Media (a
			// certificate asset in the template editor) for themselves.
			return p.Product == "" && a.privileged(p, RoleMediaPrivateRead)
		default:
			return false
		}
	case TypeURL:
		return a.allowURL(p, r, action)
	case TypeFormLink:
		// A form's link is not owned by a person: the forms service checks the
		// form role before it calls, so only that service and URL moderators
		// may read or change one.
		if action != Read && action != Create && action != Update {
			return false
		}
		return a.privilegedGroupOnly(p, RoleURLModerator) || hasRole(p, "url:forms", RoleURLModerator)
	case TypeCertificate:
		return a.allowCertificate(p, r, action)
	case TypeCertificateTemplate:
		return a.allowCertificateTemplate(p, r, action)
	case TypeGithubActivity:
		// Internal club data whose private repositories' totals are not
		// public: privileged people only, never a product's service account.
		return action == Read && p.Product == "" && a.privileged(p, RoleGithubActivityRead)
	case TypeTeam:
		return action == Read
	case TypeGroup:
		return a.privileged(p, RoleGroupsManage)
	case TypeUser:
		if action == Read && hasRole(p, "users:read") {
			return true
		}
		return a.privileged(p, RoleUsersManage)
	default:
		return false
	}
}

func (a *authorizer) allowEvent(p Principal, r Resource, action Action) bool {
	if action == Read {
		return true
	}
	if action == Assign {
		return a.privileged(p, RoleEventManage)
	}
	if action != Create && action != Update && action != Delete {
		return false
	}
	if a.privileged(p, RoleEventManage) {
		return true
	}

	owner := r.OwnerTeam
	levels := a.ownerLevels(p, owner)
	needed := a.requiredLevels(owner, action)
	for _, level := range levels {
		if slices.Contains(needed, level) {
			return true
		}
	}
	return false
}

func (a *authorizer) allowCompetitor(p Principal, r Resource, action Action) bool {
	if a.privileged(p, RoleCompetitorManage) {
		switch action {
		case Read, ReadMe, Create, Update, Delete:
			return true
		}
	}
	authenticated := p.ID != ""
	switch action {
	case Read:
		if !authenticated {
			return false
		}
		if r.OwnerID != "" && r.OwnerID == p.ID {
			return true
		}
		if r.OwnerTeam != "" {
			return a.isOwnerMember(p, r)
		}
		return false
	case ReadMe:
		return authenticated
	case Create:
		if authenticated && r.OwnerID != "" && r.OwnerID == p.ID {
			return true
		}
		return a.isOwnerMember(p, r)
	case Update:
		return a.isOwnerMember(p, r)
	case Delete:
		if authenticated && r.OwnerID != "" && r.OwnerID == p.ID {
			return true
		}
		return a.isOwnerMember(p, r)
	default:
		return false
	}
}

func (a *authorizer) isOwnerMember(p Principal, r Resource) bool {
	return len(a.ownerLevels(p, r.OwnerTeam)) > 0
}

func (a *authorizer) allowMedia(p Principal, r Resource, action Action) bool {
	switch action {
	case Read:
		return true
	case List:
		return a.privileged(p, RoleMediaManage)
	case Upload:
		if p.ID == "" {
			return false
		}
		rule := r.MediaUploader
		if rule == "" {
			rule = MediaUploaderAuthenticated
		}
		decide, ok := mediaUploaders[rule]
		return ok && decide(a, p, r)
	case Delete:
		return a.privileged(p, RoleMediaManage)
	default:
		return false
	}
}

// mediaUploaders decides every MediaUploader rule for a signed-in caller, and
// is the list of rules the Media purpose catalogue may name (Known).
var mediaUploaders = map[MediaUploader]func(a *authorizer, p Principal, r Resource) bool{
	MediaUploaderAuthenticated: func(*authorizer, Principal, Resource) bool { return true },
	MediaUploaderEventEditor: func(a *authorizer, p Principal, _ Resource) bool {
		return a.forSomeOwnerTeam(p, func(team string) bool {
			return a.allowEvent(p, Resource{Type: TypeEvent, OwnerTeam: team}, Create)
		})
	},
	MediaUploaderCertificateTemplateEditor: func(a *authorizer, p Principal, _ Resource) bool {
		return a.forSomeOwnerTeam(p, func(team string) bool {
			return a.allowCertificateTemplate(p, Resource{Type: TypeCertificateTemplate, OwnerTeam: team}, Create)
		})
	},
	// No person, whatever roles they hold: the service account of the
	// product that owns the purpose, with the role that manages its Media
	// attachments. Core owns no service account here (ServiceProducts).
	MediaUploaderServiceOnly: func(_ *authorizer, p Principal, r Resource) bool {
		return isServiceProduct(p.Product) && p.Product == r.MediaOwner && hasRole(p, "media:attach")
	},
}

// forSomeOwnerTeam reports whether allow holds for any Owner team p may act
// for, or for a record without one. An upload names no Owner team yet, so
// the candidates are the names in p's group paths, leader subgroups aside;
// the decision itself stays the one allow makes for a real record of that
// team. A record without an Owner team is the Privileged answer, which a
// role holder in no Group at all also gets.
func (a *authorizer) forSomeOwnerTeam(p Principal, allow func(team string) bool) bool {
	if allow("") {
		return true
	}
	for _, team := range a.ownerTeamCandidates(p) {
		if allow(team) {
			return true
		}
	}
	return false
}

// ownerTeamCandidates are the Owner teams p is a member or Leader of: the
// names in p's group paths, leader subgroups aside, in path order.
func (a *authorizer) ownerTeamCandidates(p Principal) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range p.Groups {
		for _, team := range strings.Split(strings.Trim(group, "/"), "/") {
			if team == "" || seen[team] || slices.Contains(a.policy.LeaderSubgroups, team) {
				continue
			}
			seen[team] = true
			out = append(out, team)
		}
	}
	return out
}

func (a *authorizer) allowTicket(p Principal, r Resource, action Action) bool {
	authenticated := p.ID != ""
	switch action {
	case Create, ReadMe:
		return authenticated
	case Read:
		if a.privileged(p, RoleTicketManage) {
			return true
		}
		if len(a.ownerLevels(p, r.OwnerTeam)) > 0 && hasRole(p, "certificate:issue") {
			return true
		}
		if slices.Contains(a.ownerLevels(p, r.OwnerTeam), LevelLeader) {
			return true
		}
		needed := a.requiredLevels(r.OwnerTeam, Update)
		for _, level := range a.ownerLevels(p, r.OwnerTeam) {
			if slices.Contains(needed, level) {
				return true
			}
		}
		return false
	case Assign:
		if a.privileged(p, RoleTicketManage) {
			return true
		}
		return slices.Contains(a.ownerLevels(p, r.OwnerTeam), LevelLeader)
	case Validate:
		return a.allowDoorCheckIn(p, r)
	default:
		return false
	}
}

func (a *authorizer) allowCertificate(p Principal, r Resource, action Action) bool {
	if action != Issue && action != Revoke && action != Read {
		return false
	}
	if action == Read {
		if a.privileged(p, RoleCertificateManage) {
			return true
		}
		if r.OwnerID != "" && r.OwnerID == p.ID {
			return true
		}
		levels := a.ownerLevels(p, r.OwnerTeam)
		if slices.Contains(levels, LevelLeader) {
			return true
		}
		return len(levels) > 0 && hasRole(p, "certificate:issue", "certificate:revoke")
	}
	if a.privileged(p, RoleCertificateManage) {
		return true
	}
	if r.OwnerTeam == "" {
		return false
	}
	levels := a.ownerLevels(p, r.OwnerTeam)
	if slices.Contains(levels, LevelLeader) {
		return true
	}
	if len(levels) == 0 {
		return false
	}
	if action == Issue {
		return hasRole(p, "certificate:issue")
	}
	return hasRole(p, "certificate:revoke")
}

func (a *authorizer) allowCertificateTemplate(p Principal, r Resource, action Action) bool {
	if a.privileged(p, RoleCertificateManage) {
		return true
	}
	if r.OwnerTeam == "" {
		return action == Read && (isAnyLeader(p) || (isAnyTeamMember(p) && hasRole(p, "certificate:template:manage", "certificate:binding:manage")))
	}
	levels := a.ownerLevels(p, r.OwnerTeam)
	if len(levels) == 0 {
		return false
	}
	if slices.Contains(levels, LevelLeader) {
		return action == Read || action == Create || action == Update || action == Assign
	}
	switch action {
	case Read:
		return hasRole(p, "certificate:template:manage", "certificate:binding:manage")
	case Create, Update:
		return hasRole(p, "certificate:template:manage")
	case Assign:
		return hasRole(p, "certificate:binding:manage")
	default:
		return false
	}
}

func isAnyLeader(p Principal) bool {
	for _, group := range p.Groups {
		upper := strings.ToUpper(group)
		if strings.Contains(upper, "/LIDERLER") || strings.Contains(upper, "/KOORDINATORLER") {
			return true
		}
	}
	return false
}

func isAnyTeamMember(p Principal) bool {
	for _, group := range p.Groups {
		if strings.Contains(strings.ToUpper(group), "/UYELER/") {
			return true
		}
	}
	return false
}

func (a *authorizer) allowDoorCheckIn(p Principal, r Resource) bool {
	if a.privileged(p, RoleTicketValidate) {
		return true
	}
	if p.ID != "" && slices.Contains(r.DoorStaffIDs, p.ID) {
		return true
	}
	levels := a.ownerLevels(p, r.OwnerTeam)
	if slices.Contains(levels, LevelLeader) {
		return true
	}
	return r.TeamDoorScan && slices.Contains(levels, LevelMember)
}

func (a *authorizer) allowURL(p Principal, r Resource, action Action) bool {
	// url:moderator and url:access, read below as before, together grant
	// every short-link action a Privileged member has.
	if a.privilegedGroupOnly(p, RoleURLModerator, RoleURLAccess) {
		return true
	}
	switch action {
	case Create:
		return p.ID != "" && hasRole(p, "url:create", "url:access")
	case ReadMe:
		return p.ID != "" && (hasRole(p, "url:get", "url:access") || hasRole(p, "url:moderator"))
	case Read:
		return hasRole(p, "url:moderator")
	case Update:
		if hasRole(p, "url:moderator") {
			return true
		}
		if r.OwnerID == "" || r.OwnerID != p.ID {
			return false
		}
		return hasRole(p, "url:update", "url:access")
	case Delete:
		if hasRole(p, "url:moderator") {
			return true
		}
		if r.OwnerID == "" || r.OwnerID != p.ID {
			return false
		}
		return hasRole(p, "url:delete", "url:access")
	default:
		return false
	}
}

func hasRole(p Principal, names ...string) bool {
	for _, want := range names {
		for _, r := range p.Roles {
			if r == want {
				return true
			}
		}
	}
	return false
}

func (a *authorizer) Permitted(p Principal, role string) bool {
	return a.privileged(p, role)
}

// privileged is the Privileged answer of a check that role stands for
// (roles.go): p's Privileged Group in the groups mode, role in the roles
// mode, either in the both mode, which counts every check where the two
// disagree. A role that existed before the contract (url:moderator,
// url:access) counts in every mode, as it always has, and holding it
// outside the Privileged Groups is no disagreement. A service account
// never holds a role here: no role makes it Privileged.
func (a *authorizer) privileged(p Principal, role string) bool {
	byRole := hasRole(p, role) && !p.ServiceAccount
	switch a.mode {
	case RoleModeRoles:
		return byRole
	case RoleModeBoth:
		group := a.privilegedGroup(p)
		if (group != "") != byRole && (group != "" || !preexistingRole(role)) {
			a.metrics.record(role, group, p.Client)
		}
		return group != "" || byRole
	default:
		return a.privilegedGroup(p) != "" || (byRole && preexistingRole(role))
	}
}

// privilegedGroupOnly is the Privileged Group shortcut of a check whose
// roles are read on their own after it (short links, form links): p's
// Privileged Group in the groups and both modes, nothing in the roles mode.
// The both mode counts a Privileged member who lacks one of roles, as
// privileged does.
func (a *authorizer) privilegedGroupOnly(p Principal, roles ...string) bool {
	if a.mode == RoleModeRoles {
		return false
	}
	group := a.privilegedGroup(p)
	if group != "" && a.mode == RoleModeBoth {
		for _, role := range roles {
			if !hasRole(p, role) || p.ServiceAccount {
				a.metrics.record(role, group, p.Client)
			}
		}
	}
	return group != ""
}

// privilegedGroup is the Privileged Group (ADMIN, YK or DK) p is in, itself
// or through a subgroup, or "".
func (a *authorizer) privilegedGroup(p Principal) string {
	for _, g := range p.Groups {
		for _, pg := range a.policy.PrivilegedGroups {
			if strings.HasSuffix(g, "/"+pg) || strings.Contains(g, "/"+pg+"/") {
				return pg
			}
		}
	}
	return ""
}

func (a *authorizer) ownerLevels(p Principal, owner string) []Level {
	if owner == "" {
		return nil
	}

	var leader, member bool
	for _, g := range p.Groups {
		for _, sub := range a.policy.LeaderSubgroups {
			if strings.Contains(g, "/"+owner+"/"+sub) {
				leader = true
			}
		}
		if strings.HasSuffix(g, "/"+owner) || strings.Contains(g, "/"+owner+"/") {
			member = true
		}
	}

	out := make([]Level, 0, 2)
	if leader {
		out = append(out, LevelLeader)
	}
	if member {
		out = append(out, LevelMember)
	}
	return out
}

func (a *authorizer) requiredLevels(ownerTeam string, action Action) []Level {
	table, ok := a.policy.EventPermissions[ownerTeam]
	if !ok {
		table = a.policy.EventPermissions["_default"]
	}
	levels, ok := table[action]
	if !ok {
		return a.policy.EventPermissions["_default"][action]
	}
	return levels
}
