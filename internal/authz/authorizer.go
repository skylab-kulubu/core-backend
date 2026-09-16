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
	case TypeEvent:
		return a.allowEvent(p, r, action)
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
