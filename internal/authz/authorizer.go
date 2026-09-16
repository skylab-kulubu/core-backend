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
	case TypeTeam:
		return action == Read
	case TypeGroup, TypeUser:
		return a.isPrivileged(p)
	default:
		return false
	}
}

func (a *authorizer) allowEvent(p Principal, r Resource, action Action) bool {
	if action == Read {
		return true
	}
	if action != Create && action != Update && action != Delete {
		return false
	}
	if a.isPrivileged(p) {
		return true
	}

	owner := r.OwnerTeam
	if owner == "" {
		owner = r.EventType
	}

	levels := a.ownerLevels(p, owner)
	needed := a.requiredLevels(r.EventType, action)
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
		return true
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
	owner := r.OwnerTeam
	if owner == "" {
		owner = r.EventType
	}
	return len(a.ownerLevels(p, owner)) > 0
}

func (a *authorizer) allowMedia(p Principal, r Resource, action Action) bool {
	switch action {
	case Read:
		return true
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
	case Read, Validate:
		if a.isPrivileged(p) {
			return true
		}
		return slices.Contains(a.ownerLevels(p, r.OwnerTeam), LevelLeader)
	default:
		return false
	}
}

func (a *authorizer) allowURL(p Principal, r Resource, action Action) bool {
	if a.isPrivileged(p) {
		return true
	}
	switch action {
	case Create:
		return p.ID != "" && (hasRole(p, "url:create", "url:access", "skylapp:url:create", "skylapp:access"))
	case ReadMe:
		return p.ID != "" && (hasRole(p, "url:get", "url:access", "skylapp:url:get", "skylapp:access") || hasRole(p, "url:moderator", "skylapp:moderator"))
	case Read:
		return hasRole(p, "url:moderator", "skylapp:moderator")
	case Update:
		if hasRole(p, "url:moderator", "skylapp:moderator") {
			return true
		}
		if r.OwnerID == "" || r.OwnerID != p.ID {
			return false
		}
		return hasRole(p, "url:update", "url:access", "skylapp:url:update", "skylapp:access")
	case Delete:
		if hasRole(p, "url:moderator", "skylapp:moderator") {
			return true
		}
		if r.OwnerID == "" || r.OwnerID != p.ID {
			return false
		}
		return hasRole(p, "url:delete", "url:access", "skylapp:url:delete", "skylapp:access")
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

func (a *authorizer) requiredLevels(eventType string, action Action) []Level {
	table, ok := a.policy.EventPermissions[eventType]
	if !ok {
		table = a.policy.EventPermissions["_default"]
	}
	levels, ok := table[action]
	if !ok {
		return a.policy.EventPermissions["_default"][action]
	}
	return levels
}
