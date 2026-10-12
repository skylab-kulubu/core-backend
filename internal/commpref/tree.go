package commpref

import (
	"slices"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
)

// TeamNode is one team of the tree: a Group teammail.TeamOf calls a team's
// own Group (not a leader subgroup, not under a Privileged root, at least
// /UYELER/<area>/<team>).
type TeamNode struct {
	ID   string
	Path string
	// Code is the Group's name (the team code); DisplayName its Turkish
	// display name, the code when it has none.
	Code        string
	DisplayName string
	// ParentID is the team directly above, "" for a top team.
	ParentID string
	// Public is public_listing=true: the team may be followed (Karar D7).
	Public bool
	// Children are the teams directly below, by path.
	Children []string
}

// Tree is the team tree as Keycloak has it now. Preferences name teams by
// Group id, so a renamed or moved team keeps them, and inheritance follows
// the tree of the moment.
type Tree struct {
	teams  map[string]*TeamNode
	roots  []string
	byPath map[string]string
	prv    []string
}

// NewTree builds the tree from every Group (Directory.ListGroups) and the
// Privileged roots, which hold no team.
func NewTree(groups []identity.Group, privilegedRoots []string) *Tree {
	t := &Tree{teams: map[string]*TeamNode{}, byPath: map[string]string{}, prv: privilegedRoots}
	for _, g := range groups {
		team, ok := teammail.TeamOf(g.Path, privilegedRoots)
		if !ok || team.Role != "" || team.Path != g.Path || g.ID == "" {
			continue
		}
		code := strings.TrimSpace(g.Name)
		if code == "" {
			code = g.Path[strings.LastIndex(g.Path, "/")+1:]
		}
		display := strings.TrimSpace(g.Attributes["display_name_tr"])
		if display == "" {
			display = code
		}
		t.teams[g.ID] = &TeamNode{
			ID: g.ID, Path: g.Path, Code: code, DisplayName: display,
			Public: strings.EqualFold(strings.TrimSpace(g.Attributes["public_listing"]), "true"),
		}
		t.byPath[g.Path] = g.ID
	}
	for _, node := range t.teams {
		node.ParentID = t.parentTeam(node.Path)
		if node.ParentID == "" {
			t.roots = append(t.roots, node.ID)
		} else {
			parent := t.teams[node.ParentID]
			parent.Children = append(parent.Children, node.ID)
		}
	}
	byPath := func(a, b string) int { return strings.Compare(t.teams[a].Path, t.teams[b].Path) }
	slices.SortFunc(t.roots, byPath)
	for _, node := range t.teams {
		slices.SortFunc(node.Children, byPath)
	}
	return t
}

// parentTeam is the nearest team above path, "" when there is none.
func (t *Tree) parentTeam(path string) string {
	for {
		cut := strings.LastIndex(path, "/")
		if cut <= 0 {
			return ""
		}
		path = path[:cut]
		if id, ok := t.byPath[path]; ok {
			return id
		}
	}
}

// Team is the team with Group id id.
func (t *Tree) Team(id string) (TeamNode, bool) {
	node, ok := t.teams[id]
	if !ok {
		return TeamNode{}, false
	}
	return *node, true
}

// chain is id and every team above it, nearest first.
func (t *Tree) chain(id string) []string {
	var out []string
	for node, ok := t.teams[id]; ok; node, ok = t.teams[node.ParentID] {
		out = append(out, node.ID)
	}
	return out
}

// root is the top team of id's chain.
func (t *Tree) root(id string) string {
	chain := t.chain(id)
	if len(chain) == 0 {
		return ""
	}
	return chain[len(chain)-1]
}

// visible reports whether id or a team above it is listed publicly: a
// team that may be followed, with its sub-teams.
func (t *Tree) visible(id string) bool {
	for _, link := range t.chain(id) {
		if t.teams[link].Public {
			return true
		}
	}
	return false
}

// Membership is what a person's Groups make them: a Member (in /UYELER),
// and a member of which teams. A sub-team's member is a member of every
// team above it (teammail.TeamOf's subtree rule); a leader subgroup's member
// is a member of its team.
type Membership struct {
	Member bool
	Teams  map[string]bool
}

// Membership reads the person's Group paths.
func (t *Tree) Membership(groupPaths []string) Membership {
	m := Membership{Teams: map[string]bool{}}
	for _, path := range groupPaths {
		if path == authz.MembersRoot || strings.HasPrefix(path, authz.MembersRoot+"/") {
			m.Member = true
		}
		team, ok := teammail.TeamOf(path, t.prv)
		if !ok {
			continue
		}
		id, ok := t.byPath[team.Path]
		if !ok {
			continue
		}
		for _, link := range t.chain(id) {
			m.Teams[link] = true
		}
	}
	return m
}

// Explicit is a person's explicit choices on one channel.
type Explicit map[Category]bool

// ExplicitOf indexes the settings of channel ch.
func ExplicitOf(settings []Setting, ch Channel) Explicit {
	out := Explicit{}
	for _, s := range settings {
		if s.Channel == ch {
			out[s.Category] = s.Enabled
		}
	}
	return out
}

// Origin is where a category's value comes from.
type Origin string

const (
	// OriginExplicit is the person's choice on the category itself.
	OriginExplicit Origin = "explicit"
	// OriginInherited is the person's choice on a team above (Karar D5).
	OriginInherited Origin = "inherited"
	// OriginDefault is the default: club and own teams (with every team
	// below them) on, others off.
	OriginDefault Origin = "default"
)

// Value is a category's value for a person.
type Value struct {
	// Enabled is the category's own value, the master switch not applied:
	// what the screen shows, greyed out while the master switch is off.
	Enabled bool
	Origin  Origin
	// From is the team the value comes from when it is not the category's
	// own: the team above whose explicit choice it inherits, or, for a
	// default, the team above the person is in.
	From string
	// Effective is whether the mail goes: Enabled and the master switch
	// on.
	Effective bool
}

// Resolve is the value of category c for a person with explicit choices e
// (of one channel) and membership m:
//
//  1. the master switch off turns everything off (the choices stay);
//  2. else, for a team, the most specific explicit choice of the kind from
//     the team up through the teams above it wins;
//  3. else the default: club-wide on for a Member; a team on when the
//     person is in it, in a team below it, or in a team above it (Y1: a
//     team's subscriber gets its sub-teams' mail too, and may turn a
//     sub-team off on its own); off otherwise.
//
// A team that is no longer in the tree is off.
func (t *Tree) Resolve(e Explicit, m Membership, c Category) Value {
	master := true
	if v, ok := e[Master()]; ok {
		master = v
	}
	var v Value
	switch c.Scope {
	case ScopeAll:
		_, explicit := e[c]
		v = Value{Enabled: master, Origin: OriginDefault}
		if explicit {
			v.Origin = OriginExplicit
		}
		v.Effective = master
		return v
	case ScopeClub:
		if on, ok := e[c]; ok {
			v = Value{Enabled: on, Origin: OriginExplicit}
		} else {
			v = Value{Enabled: m.Member, Origin: OriginDefault}
		}
	case ScopeTeam:
		v = Value{Origin: OriginDefault}
		chain := t.chain(c.GroupID)
		decided := false
		for i, id := range chain {
			if on, ok := e[Team(id, c.Kind)]; ok {
				v = Value{Enabled: on, Origin: OriginExplicit}
				if i > 0 {
					v.Origin, v.From = OriginInherited, id
				}
				decided = true
				break
			}
		}
		if !decided {
			// The nearest team of the chain the person is in: the team
			// itself, or one above it.
			for i, id := range chain {
				if m.Teams[id] {
					v.Enabled = true
					if i > 0 {
						v.From = id
					}
					break
				}
			}
		}
	}
	v.Effective = master && v.Enabled
	return v
}
