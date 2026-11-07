package identity

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The roles of a person in a Team (PersonTeam.Role).
const (
	// TeamRoleLeader: directly in the Team's Leader group (LIDERLER or
	// KOORDINATORLER) or in one of a Team above it.
	TeamRoleLeader = "leader"
	// TeamRoleMember: in the Team's Group or a Group under it, and no
	// Leader of it.
	TeamRoleMember = "member"
)

// PersonTeam is a Team a person is in or leads (GET
// /internal/v1/users/{sub}/teams, docs/team-delegation.md "A person's
// Teams"): who SkyMail lets send Team mail to it (ADR-0067 addendum of
// 2026-11-08), and the Teams Forms offers as a form's owner team (ADR-0075
// addendum of 2026-11-08). It names no other person and counts nobody.
type PersonTeam struct {
	ID          string        `json:"id"`
	Path        string        `json:"path"`
	Code        string        `json:"code"`
	DisplayName LocalizedText `json:"displayName"`
	Role        string        `json:"role"`
}

type peopleEntry struct {
	teams   []PersonTeam
	expires time.Time
}

// errNoDelegation is a person's Teams asked of no TeamDelegation (a
// directory without TeamReads).
var errNoDelegation = errors.New("identity: team reads are not configured")

// PersonTeams are the Teams userID leads, with every Sub-team below them
// (TeamRoleLeader), and the Teams they are otherwise in, with every Team
// above (TeamRoleMember), in path order, each once. Read with delegation on
// or off: the answer changes nobody's rights in Keycloak, so TEAM_DELEGATION
// does not gate it. Who leads what is decided as for ManagedTeams (the
// Leader group right under its Team by id and by path, a Privileged root
// no Team); protection does not matter here. Read from Keycloak now
// (ADR-0065 karar 3), kept TTL and dropped by Forget; a failed read is an
// error, never a partial answer, and is not kept.
func (d *TeamDelegation) PersonTeams(ctx context.Context, userID uuid.UUID) ([]PersonTeam, error) {
	if d == nil {
		return nil, errNoDelegation
	}
	d.mu.Lock()
	entry, ok := d.people[userID]
	generation := d.generation
	d.mu.Unlock()
	if ok && d.options.Now().Before(entry.expires) {
		return entry.teams, nil
	}
	type answer struct {
		teams []PersonTeam
		err   error
	}
	key := "people:" + userID.String() + "@" + strconv.FormatUint(generation, 10)
	v, _, _ := d.flights.Do(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.options.Timeout)
		defer cancel()
		teams, err := d.readPersonTeams(ctx, userID)
		if err != nil {
			return answer{nil, err}, nil
		}
		d.keep(generation, func() {
			d.people[userID] = peopleEntry{teams: teams, expires: d.options.Now().Add(d.options.TTL)}
		})
		return answer{teams, nil}, nil
	})
	got := v.(answer)
	return got.teams, got.err
}

func (d *TeamDelegation) readPersonTeams(ctx context.Context, userID uuid.UUID) ([]PersonTeam, error) {
	groups, err := d.dir.GroupsForUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return []PersonTeam{}, nil
	}
	if err != nil {
		return nil, err
	}
	leaders, err := d.leaderGroupsIn(ctx, groups)
	if err != nil {
		return nil, err
	}
	led, err := d.managedTeamsOf(ctx, leaders)
	if err != nil {
		return nil, err
	}
	found := map[string]PersonTeam{}
	starts := make([]string, 0, len(groups)+len(led))
	for _, team := range led {
		found[team.ID] = PersonTeam{ID: team.ID, Path: team.Path, Code: team.Code, DisplayName: team.DisplayName, Role: TeamRoleLeader}
		if err := d.leaderSubtree(ctx, team.ID, team.Path, 0, found); err != nil {
			return nil, err
		}
		starts = append(starts, team.ID)
	}
	// A Team Group the person is directly in; their Leader groups count
	// through the Teams they lead, so one that only looks like a Leader
	// group makes them a member of nothing.
	for _, g := range groups {
		if d.isTeam(nodeOf(g, "")) {
			starts = append(starts, g.ID)
		}
	}
	for _, id := range starts {
		if err := d.memberChain(ctx, id, found); err != nil {
			return nil, err
		}
	}
	out := make([]PersonTeam, 0, len(found))
	for _, team := range found {
		out = append(out, team)
	}
	slices.SortFunc(out, func(a, b PersonTeam) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// leaderSubtree marks every Team below the Team id at path as led, walked
// down by id; Leader groups and Groups whose path lies are left out.
func (d *TeamDelegation) leaderSubtree(ctx context.Context, id, path string, depth int, found map[string]PersonTeam) error {
	if depth >= maxGroupDepth {
		return nil
	}
	children, err := d.children(ctx, nodeOf(Group{ID: id, Path: path}, ""))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, child := range children {
		if slices.Contains(d.policy.LeaderSubgroups, child.Name) || !d.isTeam(child) {
			continue
		}
		full, _, err := d.reads.GroupNode(ctx, child.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		found[child.ID] = PersonTeam{ID: child.ID, Path: child.Path, Code: child.Name, DisplayName: displayName(full), Role: TeamRoleLeader}
		if err := d.leaderSubtree(ctx, child.ID, child.Path, depth+1, found); err != nil {
			return err
		}
	}
	return nil
}

// memberChain marks the Team id and every Team above it, read by id up to
// the first Group that is no Team, as the person's (a member where not
// led). A Group whose path does not continue its parent's ends the chain.
func (d *TeamDelegation) memberChain(ctx context.Context, id string, found map[string]PersonTeam) error {
	var below Group
	for depth := 0; id != "" && depth < maxGroupDepth; depth++ {
		g, parent, err := d.reads.GroupNode(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if g.Name == "" || strings.Contains(g.Name, "/") || !strings.HasSuffix(g.Path, "/"+g.Name) || !d.isTeam(nodeOf(g, parent)) {
			return nil
		}
		if below.ID != "" && below.Path != g.Path+"/"+below.Name {
			return nil
		}
		if _, ok := found[g.ID]; !ok {
			found[g.ID] = PersonTeam{ID: g.ID, Path: g.Path, Code: g.Name, DisplayName: displayName(g), Role: TeamRoleMember}
		}
		below, id = g, parent
	}
	return nil
}
