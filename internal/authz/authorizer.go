package authz

import (
	"slices"
	"strings"
)

type Authorizer interface {
	Allow(p Principal, r Resource, a Action) bool
}

type authorizer struct {
	policy Policy
}

func NewAuthorizer(policy Policy) Authorizer {
	return &authorizer{policy: policy}
}

func (a *authorizer) Allow(p Principal, r Resource, action Action) bool {
	switch r.Type {
	case TypeEvent, TypeEventDay, TypeSession:
		return a.allowEvent(p, r, action)
	case TypeSeason:
		if action == Read {
			return true
		}
		return a.isPrivileged(p)
	case TypeTicket:
		return a.allowTicket(p, r, action)
	case TypeCompetitor:
		return a.allowCompetitor(p, r, action)
	case TypeMedia:
		return a.allowMedia(p, r, action)
	case TypeURL:
		return a.allowURL(p, r, action)
	case TypeCertificate:
		return a.allowCertificate(p, r, action)
	case TypeCertificateTemplate:
		return a.allowCertificateTemplate(p, r, action)
	case TypeTeam:
		return action == Read
	case TypeGroup:
		return a.isPrivileged(p)
	case TypeUser:
		if action == Read && hasRole(p, "users:read") {
			return true
		}
		return a.isPrivileged(p)
	default:
		return false
	}
}

func (a *authorizer) allowEvent(p Principal, r Resource, action Action) bool {
	if action == Read {
		return true
	}
	if action == Assign {
		return a.isPrivileged(p)
	}
	if action != Create && action != Update && action != Delete {
		return false
	}
	if a.isPrivileged(p) {
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
	if a.isPrivileged(p) {
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
		return a.isPrivileged(p)
	case Upload:
		return p.ID != ""
	case Delete:
		return a.isPrivileged(p)
	default:
		return false
	}
}

func (a *authorizer) allowTicket(p Principal, r Resource, action Action) bool {
	authenticated := p.ID != ""
	switch action {
	case Create, ReadMe:
		return authenticated
	case Read:
		if a.isPrivileged(p) {
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
		if a.isPrivileged(p) {
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
		if a.isPrivileged(p) {
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
	if a.isPrivileged(p) {
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
	if a.isPrivileged(p) {
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
	if a.isPrivileged(p) {
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
	if a.isPrivileged(p) {
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

func (a *authorizer) isPrivileged(p Principal) bool {
	for _, g := range p.Groups {
		for _, pg := range a.policy.PrivilegedGroups {
			if strings.HasSuffix(g, "/"+pg) || strings.Contains(g, "/"+pg+"/") {
				return true
			}
		}
	}
	return false
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
