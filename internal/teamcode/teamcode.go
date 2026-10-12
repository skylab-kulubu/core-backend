// Package teamcode is the rule for the names of the Groups core creates and
// renames (docs/team-codes.md). A Group's Keycloak name is its Team code:
// Owner team records, the CMS's Teams and Forms' templates use it as a key,
// and core's Leader and Owner team decisions read it from the token's group
// paths. So a new name is a plain ASCII code, never one of the names core's
// rules give a meaning to (ADMIN, LIDERLER, …), and taken nowhere else: not
// by another Group, not by a record of core, not by a code given up before.
package teamcode

import (
	"regexp"
	"slices"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// The problem codes (RFC 7807 code member) of a refused name.
const (
	// InvalidName: the name is not a code (Pattern).
	InvalidName = "invalid_name"
	// ReservedName: the name is one core's rules give a meaning to, and the
	// Group is not at the one place such a Group may be.
	ReservedName = "reserved_name"
	// NameTaken: another Group, a record of core or a retired code holds the
	// name, compared without case.
	NameTaken = "name_taken"
	// PrivilegedRoot: a rename would move or make a Privileged root.
	PrivilegedRoot = "privileged_root"
)

// Error is a refused name. Detail says why, for the caller; it names no
// person.
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string { return "teamcode: " + e.Code + ": " + e.Detail }

// Taken is the error for a name another Group or record holds.
func Taken(name string) error {
	return &Error{Code: NameTaken, Detail: "The name " + name + " is taken: another Group, a record of core or a retired Team code holds it (case does not count)."}
}

// Pattern is the form of a new code: an ASCII capital letter, then 1 to 23
// capital letters, digits or hyphens. Turkish letters, look-alikes and the
// path separator stay out; a team's Turkish name is its display name.
var Pattern = regexp.MustCompile(`^[A-Z][A-Z0-9-]{1,23}$`)

// Valid reports whether name has the form of a new code.
func Valid(name string) bool { return Pattern.MatchString(name) }

// fixedReserved are reserved whatever the policy says: the Privileged
// names, the members' tree, the leader subgroups, the teams' old area and
// the Event permission table's fallback.
var fixedReserved = []string{"ADMIN", "YK", "DK", "UYELER", "LIDERLER", "KOORDINATORLER", "TAKIMLAR", "_DEFAULT"}

// Scope is what a name that passed the rule must still be free of.
type Scope int

const (
	// Everywhere: no other Group of the realm, no Owner team record of core
	// and no retired code may hold the name.
	Everywhere Scope = iota
	// AtPath: only the Group's own path must be free. A reserved name at its
	// one allowed place (a Privileged root, a Team's Leader group) is the
	// same name wherever it is allowed.
	AtPath
)

// Rules is the name rule over a policy's Privileged roots, leader
// subgroups and Event permission table.
type Rules struct {
	roots    []string
	leaders  []string
	reserved []string
}

// NewRules is the rule for policy: its Privileged roots, its leader
// subgroups, and as reserved names the fixed ones, every root's name and
// every Owner team the Event permission table names (GECEKODU).
func NewRules(policy authz.Policy) Rules {
	reserved := slices.Clone(fixedReserved)
	reserved = append(reserved, policy.LeaderSubgroups...)
	for _, root := range policy.PrivilegedRoots {
		reserved = append(reserved, authz.RootLabel(root))
	}
	for code := range policy.EventPermissions {
		reserved = append(reserved, code)
	}
	return Rules{
		roots:    slices.Clone(policy.PrivilegedRoots),
		leaders:  slices.Clone(policy.LeaderSubgroups),
		reserved: reserved,
	}
}

// Reserved reports whether name is a reserved name, compared without case.
func (r Rules) Reserved(name string) bool {
	return slices.ContainsFunc(r.reserved, func(reserved string) bool { return strings.EqualFold(reserved, name) })
}

// Create checks name for a new Group under parentPath ("" for the realm's
// top), apart from whether the name is taken: the Scope says what it must
// still be free of.
//
// A reserved name is refused, except a root's name (ADMIN, YK, DK) for a
// Group that is itself a Privileged root, and a leader subgroup's name
// (LIDERLER, KOORDINATORLER) for a Group right under a Team; a new Group is
// a leaf.
func (r Rules) Create(parentPath, name string) (Scope, error) {
	return r.check(parentPath, name, true)
}

// Rename checks the new name of the Group at path, as Create does, and
// refuses a rename that would move or make a Privileged root: of a root, of
// a Group above one, or to a root's path. leaf is whether the Group has no
// subgroup; only a leaf may take a leader subgroup's name.
func (r Rules) Rename(path, name string, leaf bool) (Scope, error) {
	parentPath := parentOf(path)
	if r.touchesRoot(path) || r.touchesRoot(parentPath+"/"+name) {
		return Everywhere, &Error{Code: PrivilegedRoot, Detail: "A Privileged root, a Group above one, or a Group that would become one is not renamed: its members would gain or lose Privileged."}
	}
	return r.check(parentPath, name, leaf)
}

func (r Rules) check(parentPath, name string, leaf bool) (Scope, error) {
	if !Valid(name) {
		return Everywhere, &Error{Code: InvalidName, Detail: "A Group name is 2 to 24 ASCII capital letters, digits or hyphens and starts with a letter (WEBLAB, AI-LAB); the Turkish name goes in display_name_tr."}
	}
	if !r.Reserved(name) {
		return Everywhere, nil
	}
	path := parentPath + "/" + name
	switch {
	case authz.PrivilegedRoot(r.roots, path) == path:
		return AtPath, nil
	case slices.Contains(r.leaders, name) && leaf && r.isTeam(parentPath):
		return AtPath, nil
	}
	return Everywhere, &Error{Code: ReservedName, Detail: "The name " + name + " is reserved: a Privileged root's name only names that root, a leader subgroup's only a Team's Leader group, and the others no new Group."}
}

// touchesRoot reports whether path is a Privileged root or above one.
func (r Rules) touchesRoot(path string) bool {
	return slices.ContainsFunc(r.roots, func(root string) bool {
		return root == path || strings.HasPrefix(root, path+"/")
	})
}

func (r Rules) isTeam(path string) bool {
	team, role, ok := TeamOf(path, r.roots, r.leaders)
	return ok && role == "" && team == path
}

// TeamOf reports whether the Group at path is a Team or one of its Leader
// groups, and which: the Team's path and, for a Leader group, its name
// (role). A Team is a Group at least three deep in the members' tree
// (/UYELER/<area>/<team>, or a Sub-team below one); a Leader group is one of
// leaders right under a Team, with nothing under it. A Group under a
// Privileged root of roots is neither, nor are the members' tree itself,
// its areas, or Groups outside it.
func TeamOf(path string, roots, leaders []string) (team, role string, ok bool) {
	if !strings.HasPrefix(path, authz.MembersRoot+"/") || authz.PrivilegedRoot(roots, path) != "" {
		return "", "", false
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, segment := range segments {
		if segment == "" {
			return "", "", false
		}
		// A leader subgroup ends a path: nothing sits under one.
		if slices.Contains(leaders, segment) && i != len(segments)-1 {
			return "", "", false
		}
	}
	team = path
	if last := segments[len(segments)-1]; slices.Contains(leaders, last) {
		segments = segments[:len(segments)-1]
		team, role = "/"+strings.Join(segments, "/"), last
	}
	if len(segments) < minTeamDepth {
		return "", "", false
	}
	return team, role, true
}

// minTeamDepth is the depth of a Team's own Group: /UYELER/<area>/<team>.
// /UYELER itself is the club and /UYELER/<area> an area (ARGE,
// ORGANIZASYON, …) or a technical Group, none of them a Team.
const minTeamDepth = 3

// parentOf is the path of the Group above the one at path, "" for a Group at
// the realm's top.
func parentOf(path string) string {
	return path[:max(strings.LastIndex(path, "/"), 0)]
}
