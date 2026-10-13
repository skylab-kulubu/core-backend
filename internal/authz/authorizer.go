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
	case TypeCompetition:
		return a.allowCompetition(p, r, action)
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
			return p.Product == "" && a.privilegedRead(p, RoleMediaPrivateRead)
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
		return a.privilegedGroupOnly(p, action == Read, RoleURLModerator) || hasRole(p, "url:forms", RoleURLModerator)
	case TypeFormResponse:
		// Only the forms service reports the answers to its forms, and a
		// report writes Tickets: no person may send one, whatever roles they
		// hold.
		return action == Create && p.Product == ProductForms && hasRole(p, "ticket:forms")
	case TypeCertificate:
		return a.allowCertificate(p, r, action)
	case TypeCertificateTemplate:
		return a.allowCertificateTemplate(p, r, action)
	case TypeGithubActivity:
		// Internal club data whose private repositories' totals are not
		// public: privileged people only, never a product's service account.
		return action == Read && p.Product == "" && a.privilegedRead(p, RoleGithubActivityRead)
	case TypeTeamAudit:
		// The audit:read role alone, in every role mode: no Privileged
		// Group stands for it, global:read neither, and no service account
		// reads it. Keycloak maps it to ADMIN, YK and DK (grup-yonetimi
		// GY1, which replaced S11's "not DK").
		return action == Read && p.Product == "" && !p.ServiceAccount && hasRole(p, RoleAuditRead)
	case TypeTeam:
		return action == Read
	case TypeGroup:
		if action == Read || action == List {
			return a.privilegedRead(p, RoleGroupsManage)
		}
		return a.privileged(p, RoleGroupsManage)
	case TypeUser:
		switch action {
		case Read:
			return hasRole(p, "users:read") || a.privilegedRead(p, RoleUsersManage)
		case ReadFull:
			return a.privilegedRead(p, RoleUsersManage)
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
	switch action {
	case Read, ReadMe:
		if a.privilegedRead(p, RoleCompetitorManage) {
			return true
		}
	case Create, Update, Delete:
		if a.privileged(p, RoleCompetitorManage) {
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

// allowCompetition is the organizer of a Competition: competition:manage,
// or a Leader of the Event's Owner team (spec §8). Membership of the team
// alone manages nothing. An Event without an Owner team is Privileged only
// (ADR-0018). Who the Leaders are is ownerLevels' answer, so the team
// authority handover changes it for competitions with everything else.
func (a *authorizer) allowCompetition(p Principal, r Resource, action Action) bool {
	switch action {
	case Read:
		if a.privilegedRead(p, RoleCompetitionManage) {
			return true
		}
	case Create, Update, Delete:
		if a.privileged(p, RoleCompetitionManage) {
			return true
		}
	default:
		return false
	}
	return slices.Contains(a.ownerLevels(p, r.OwnerTeam), LevelLeader)
}

func (a *authorizer) isOwnerMember(p Principal, r Resource) bool {
	return len(a.ownerLevels(p, r.OwnerTeam)) > 0
}

func (a *authorizer) allowMedia(p Principal, r Resource, action Action) bool {
	switch action {
	case Read:
		return true
	case List:
		return a.privilegedRead(p, RoleMediaManage)
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
// names in p's group paths, leader subgroups aside, in path order. A path
// under a Privileged root names no Owner team (ownerLevels).
func (a *authorizer) ownerTeamCandidates(p Principal) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range p.Groups {
		if PrivilegedRoot(a.policy.PrivilegedRoots, group) != "" {
			continue
		}
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
		if a.privilegedRead(p, RoleTicketManage) {
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
		if a.privilegedRead(p, RoleCertificateManage) {
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
	if action == Read && a.privilegedRead(p, RoleCertificateManage) {
		return true
	}
	if action != Read && a.privileged(p, RoleCertificateManage) {
		return true
	}
	if r.OwnerTeam == "" {
		return action == Read && (a.isAnyLeader(p) || (isAnyTeamMember(p) && hasRole(p, "certificate:template:manage", "certificate:binding:manage")))
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

// isAnyLeader reports whether p is in some Leader group: a Group whose last
// segment is one of the policy's leader subgroups, under a parent. The whole
// segment counts: /UYELER/ARGE/WEBLAB/LIDERLER-ESKI is no Leader group.
func (a *authorizer) isAnyLeader(p Principal) bool {
	for _, group := range p.Groups {
		segments := pathSegments(group)
		if len(segments) >= 2 && slices.Contains(a.policy.LeaderSubgroups, segments[len(segments)-1]) {
			return true
		}
	}
	return false
}

// isAnyTeamMember reports whether p is in some Group of the members' tree
// (MembersRoot), below the root itself.
func isAnyTeamMember(p Principal) bool {
	for _, group := range p.Groups {
		if strings.HasPrefix(group, MembersRoot+"/") {
			return true
		}
	}
	return false
}

// MembersRoot is the club's members' tree: every Team sits under it
// (CONTEXT.md "Member").
const MembersRoot = "/UYELER"

// pathSegments are the names of a Group path, top first.
func pathSegments(path string) []string {
	return strings.Split(strings.TrimPrefix(path, "/"), "/")
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
	// every short-link action a Privileged member has; a Privileged reader
	// only reads (Read, ReadMe).
	if a.privilegedGroupOnly(p, action == Read || action == ReadMe, RoleURLModerator, RoleURLAccess) {
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
	if role == RoleGlobalRead {
		return a.privilegedRead(p, RoleGlobalRead)
	}
	return a.privileged(p, role)
}

// privileged is the Privileged answer of a check that role stands for
// (roles.go) when the check changes something: p's Privileged Group in the
// groups mode, role in the roles mode, either in the both mode, which
// counts every check where the two disagree. A Privileged reader root (DK,
// AUTHZ_PRIVILEGED_READER_ROOTS) and global:read never allow here. A role
// that existed before the contract (url:moderator, url:access) counts in
// every mode, as it always has, and holding it outside the Privileged
// Groups is no disagreement. A role being renamed is held under either name
// (renamedRoles). A service account never holds a role here: no role makes
// it Privileged.
func (a *authorizer) privileged(p Principal, role string) bool {
	return a.decide(p, role, false)
}

// privilegedRead is privileged for a check that only reads (Read, List):
// a Privileged reader root's member and a holder of global:read are
// allowed too, as Entra's Global Reader reads what a Global Admin reads.
func (a *authorizer) privilegedRead(p Principal, role string) bool {
	return a.decide(p, role, true)
}

func (a *authorizer) decide(p Principal, role string, read bool) bool {
	names := roleNames(role)
	if read {
		names = append(names, RoleGlobalRead)
	}
	byRole := hasRole(p, names...) && !p.ServiceAccount
	switch a.mode {
	case RoleModeRoles:
		return byRole
	case RoleModeBoth:
		group := a.privilegedGroup(p, read)
		if (group != "") != byRole && (group != "" || !preexistingRole(role)) {
			a.metrics.record(role, group, p.Client)
		}
		return group != "" || byRole
	default:
		return a.privilegedGroup(p, read) != "" || (hasRole(p, roleNames(role)...) && !p.ServiceAccount && preexistingRole(role))
	}
}

// privilegedGroupOnly is the Privileged Group shortcut of a check whose
// roles are read on their own after it (short links, form links): p's
// Privileged Group in the groups and both modes, nothing in the roles mode.
// A reader root counts only when read. The both mode counts a Privileged
// member who lacks one of roles, as privileged does.
func (a *authorizer) privilegedGroupOnly(p Principal, read bool, roles ...string) bool {
	if a.mode == RoleModeRoles {
		return false
	}
	group := a.privilegedGroup(p, read)
	if group != "" && a.mode == RoleModeBoth {
		for _, role := range roles {
			if !hasRole(p, role) || p.ServiceAccount {
				a.metrics.record(role, group, p.Client)
			}
		}
	}
	return group != ""
}

// privilegedGroup names the Privileged root p is in, itself or through a
// subgroup: the root's last segment (ADMIN, YK or DK), or "". Only the
// policy's roots count, anchored at their paths (PrivilegedRoot). A Group
// under a reader root (PrivilegedReaderRoots) counts only for a read.
func (a *authorizer) privilegedGroup(p Principal, read bool) string {
	for _, g := range p.Groups {
		root := PrivilegedRoot(a.policy.PrivilegedRoots, g)
		if root == "" {
			continue
		}
		if !read && PrivilegedRoot(a.policy.PrivilegedReaderRoots, g) != "" {
			continue
		}
		return RootLabel(root)
	}
	return ""
}

// ownerLevels are p's levels in the Owner team owner, by whole path
// segments: a member is in a Group one of whose segments is owner, a Leader
// in a leader subgroup that is the last segment, right under owner. Paths
// under a Privileged root name no Owner team: such a member is Privileged,
// which the decisions read first, or in the roles mode holds roles.
func (a *authorizer) ownerLevels(p Principal, owner string) []Level {
	if owner == "" {
		return nil
	}

	var leader, member bool
	for _, g := range p.Groups {
		if PrivilegedRoot(a.policy.PrivilegedRoots, g) != "" {
			continue
		}
		segments := pathSegments(g)
		for i, segment := range segments {
			if segment != owner {
				continue
			}
			member = true
			if i == len(segments)-2 && slices.Contains(a.policy.LeaderSubgroups, segments[i+1]) {
				leader = true
			}
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
