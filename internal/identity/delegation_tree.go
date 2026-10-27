package identity

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// Tree is the managed tree of the caller userID (SPEC §6.2): for
// Privileged every Team of the members' tree, by area; for a Leader, with
// delegation on, the Teams they lead and every Sub-team below, each Team
// once (a Sub-team they also lead sits under the Team above it); for anyone
// else nothing. Every node carries the caller's decisions on it. The tree
// is walked down by id from the Teams it starts at; an answer is kept TTL
// and dropped by core's own Group writes.
func (d *TeamDelegation) Tree(ctx context.Context, userID uuid.UUID, privileged bool) ([]ManagedNode, error) {
	return d.TreeFor(ctx, userID, privileged, privileged)
}

// TreeFor is Tree for a caller who is Privileged for reading Groups
// (privileged) and for changing them (write). A Privileged reader (DK,
// grup-yonetimi GY1) gets every Team with group.read and nothing to
// change: the tree of a Privileged member whose write decisions are all
// false.
func (d *TeamDelegation) TreeFor(ctx context.Context, userID uuid.UUID, privileged, write bool) ([]ManagedNode, error) {
	write = write && privileged
	if d == nil || (!privileged && !d.On()) {
		return []ManagedNode{}, nil
	}
	d.mu.Lock()
	entry, ok := d.trees[userID]
	d.mu.Unlock()
	if ok && entry.privileged == privileged && entry.write == write && d.options.Now().Before(entry.expires) {
		return entry.nodes, nil
	}
	var (
		nodes []ManagedNode
		err   error
	)
	if privileged {
		nodes, err = d.privilegedTree(ctx, userID, write)
	} else {
		nodes, err = d.leaderTree(ctx, userID)
	}
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.trees[userID] = treeEntry{nodes: nodes, privileged: privileged, write: write, expires: d.options.Now().Add(d.options.TTL)}
	d.mu.Unlock()
	return nodes, nil
}

// privilegedTree is every top Team (/UYELER/<area>/<team>) with what is
// below it. The members' tree is found by its path, everything under it
// by id; a Privileged root is no area. Without write (a Privileged
// reader), every node reads, and the caller changes only what they lead
// with delegation on, as a Leader outside the Privileged roots would.
func (d *TeamDelegation) privilegedTree(ctx context.Context, userID uuid.UUID, write bool) ([]ManagedNode, error) {
	root, err := d.dir.GetGroup(ctx, authz.MembersRoot)
	if errors.Is(err, ErrNotFound) {
		return []ManagedNode{}, nil
	}
	if err != nil {
		return nil, err
	}
	members, parent, err := d.reads.GroupNode(ctx, root.ID)
	if err != nil {
		return nil, err
	}
	top := nodeOf(members, parent)
	if top.ParentID != "" || top.Path != authz.MembersRoot {
		return []ManagedNode{}, nil
	}
	manager := authz.TeamManager{Privileged: true}
	if !write {
		manager = authz.TeamManager{}
		if d.On() {
			if manager, err = d.manager(ctx, userID, false); err != nil {
				return nil, err
			}
		}
	}
	out := make([]ManagedNode, 0)
	areas, err := d.children(ctx, top)
	if err != nil {
		return nil, err
	}
	for _, area := range areas {
		if authz.PrivilegedRoot(d.policy.PrivilegedRoots, area.Path) != "" {
			continue
		}
		teams, err := d.children(ctx, area)
		if err != nil {
			return nil, err
		}
		for _, team := range teams {
			if !d.isTeam(team) {
				continue
			}
			above := authz.ManagedGroup{Group: area, Ancestors: []authz.GroupNode{top}}
			above.Protected, err = d.protected(ctx, []string{area.ID, top.ID}, false)
			if err != nil {
				return nil, err
			}
			node, err := d.subtree(ctx, manager, true, team, above, 0)
			if err != nil {
				return nil, err
			}
			out = append(out, node)
		}
	}
	slices.SortFunc(out, func(a, b ManagedNode) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// leaderTree is every Team userID leads, read fresh by id up to the
// realm's top, without the ones under another Team they lead.
func (d *TeamDelegation) leaderTree(ctx context.Context, userID uuid.UUID) ([]ManagedNode, error) {
	manager, err := d.manager(ctx, userID, false)
	if err != nil {
		return nil, err
	}
	led := map[string]bool{}
	for _, leader := range manager.LeaderGroups {
		if leader.TeamID != "" {
			led[leader.TeamID] = true
		}
	}
	out := make([]ManagedNode, 0)
	for _, id := range slices.Sorted(maps.Keys(led)) {
		team, _, err := d.target(ctx, id, false)
		var refused *DelegationError
		switch {
		case errors.Is(err, ErrNotFound), errors.As(err, &refused):
			continue
		case err != nil:
			return nil, err
		}
		if slices.ContainsFunc(team.Ancestors, func(a authz.GroupNode) bool { return led[a.ID] }) {
			continue
		}
		decision := d.policy.Manages(manager, team)
		if !decision.Allowed && decision.Refusal == "" {
			continue
		}
		above := authz.ManagedGroup{Protected: team.Protected}
		if len(team.Ancestors) > 0 {
			above.Group, above.Ancestors = team.Ancestors[0], team.Ancestors[1:]
		}
		node, err := d.subtree(ctx, manager, false, team.Group, above, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	slices.SortFunc(out, func(a, b ManagedNode) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// subtree is the node of team, whose parent above is (above.Group, with its
// ancestors and whether one of them is protected), and of the Teams below
// it. readAll is a Privileged caller's group.read on every node, whether
// or not manager may change it.
func (d *TeamDelegation) subtree(ctx context.Context, manager authz.TeamManager, readAll bool, team authz.GroupNode, above authz.ManagedGroup, depth int) (ManagedNode, error) {
	own, err := d.protected(ctx, []string{team.ID}, false)
	if err != nil {
		return ManagedNode{}, err
	}
	target := authz.ManagedGroup{Group: team, Protected: own || above.Protected}
	if above.Group.ID != "" {
		target.Ancestors = append([]authz.GroupNode{above.Group}, above.Ancestors...)
	}
	full, _, err := d.reads.GroupNode(ctx, team.ID)
	if err != nil {
		return ManagedNode{}, err
	}
	members, err := d.dir.Members(ctx, team.ID)
	if err != nil {
		return ManagedNode{}, err
	}
	manages, strict := d.policy.Manages(manager, target).Allowed, d.policy.ManagesStrict(manager, target).Allowed
	node := ManagedNode{
		ID: team.ID, Code: team.Name, Path: team.Path, DisplayName: displayName(full),
		MemberCount: len(members), Protected: target.Protected,
		Can: map[string]bool{
			"group.read":     readAll || manages,
			"members.write":  d.policy.ManagesMembership(manager, target).Allowed,
			"leaders.write":  strict,
			"subteams.write": manages,
			"code.write":     strict,
			"team.delete":    strict,
		},
		Children: []ManagedNode{},
	}
	children, err := d.children(ctx, team)
	if err != nil {
		return ManagedNode{}, err
	}
	leaders := map[uuid.UUID]bool{}
	for _, child := range children {
		switch {
		case slices.Contains(d.policy.LeaderSubgroups, child.Name):
			people, err := d.dir.Members(ctx, child.ID)
			if err != nil {
				return ManagedNode{}, err
			}
			for _, p := range people {
				leaders[p.ID] = true
			}
		case d.isTeam(child) && depth < maxGroupDepth:
			sub, err := d.subtree(ctx, manager, readAll, child, target, depth+1)
			if err != nil {
				return ManagedNode{}, err
			}
			node.Children = append(node.Children, sub)
		}
	}
	node.LeaderCount = len(leaders)
	slices.SortFunc(node.Children, func(a, b ManagedNode) int { return strings.Compare(a.Path, b.Path) })
	return node, nil
}

// children are the Groups right under parent whose path is parent's and
// their own name (a name with a "/" is left out: its path lies).
func (d *TeamDelegation) children(ctx context.Context, parent authz.GroupNode) ([]authz.GroupNode, error) {
	groups, err := d.dir.Subgroups(ctx, parent.ID)
	if err != nil {
		return nil, err
	}
	out := make([]authz.GroupNode, 0, len(groups))
	for _, g := range groups {
		if g.ID == "" || g.Name == "" || strings.Contains(g.Name, "/") || g.Path != parent.Path+"/"+g.Name {
			continue
		}
		out = append(out, authz.GroupNode{ID: g.ID, Name: g.Name, Path: g.Path, ParentID: parent.ID})
	}
	return out, nil
}

func (d *TeamDelegation) isTeam(node authz.GroupNode) bool {
	team, role, ok := authz.TeamOf(node.Path, d.policy.PrivilegedRoots, d.policy.LeaderSubgroups)
	return ok && role == "" && team == node.Path
}
