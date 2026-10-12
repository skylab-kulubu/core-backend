package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"golang.org/x/sync/singleflight"
)

// GroupNodes reads one Group by its Keycloak id with the id of the Group
// above it ("" at the realm's top). Anything but an id is ErrNotFound: a
// delegated decision never resolves a Group by its name or path.
type GroupNodes interface {
	GroupNode(ctx context.Context, id string) (Group, string, error)
}

// The problem codes of a delegated request core refuses, beside authz's
// (authz.RefusalProtectedGroup, authz.RefusalLeaderGroupNeedsParent).
const (
	// CodeGroupLimit: the add would put the person in more Groups directly
	// than a delegated add may (SPEC §4.5).
	CodeGroupLimit = "group_limit"
	// CodeGroupTreeMismatch: the Group's ancestors by id and by path do not
	// agree (a name with a "/", a tree changed mid-read). Logged.
	CodeGroupTreeMismatch = "group_tree_mismatch"
	// CodeTeamCodeNotUnique: a Group on the way to the target shares its
	// name with another Group of the realm, so a membership of it would
	// read as a membership of the other Team too (Owner team decisions read
	// names). Only Privileged writes there until the names are fixed.
	CodeTeamCodeNotUnique = "team_code_not_unique"
	// CodeRateLimited: the caller made more delegated writes this minute
	// than a delegated caller may.
	CodeRateLimited = "rate_limited"
)

// DelegationError is a delegated request refused with a problem code and
// the status it is answered with.
type DelegationError struct {
	Status int
	Code   string
}

func (e *DelegationError) Error() string {
	return fmt.Sprintf("identity: delegation refused: %s (%d)", e.Code, e.Status)
}

func refusedWith(status int, code string) error { return &DelegationError{Status: status, Code: code} }

// Delegation defaults (SPEC §4.5 and §6.2).
const (
	DefaultDelegatedGroupLimit = 25
	DefaultDelegatedWrites     = 60
	defaultDelegationTTL       = time.Minute
	defaultDelegationFailTTL   = 10 * time.Second
	defaultDelegationTimeout   = 3 * time.Second
	// maxGroupDepth bounds the walk up a Group's ancestors: a parent chain
	// longer than any real tree is a loop or a fault.
	maxGroupDepth = 32
)

// TeamDelegationOptions tune a TeamDelegation. Zero values take the
// defaults.
type TeamDelegationOptions struct {
	// GroupLimit is how many Groups a delegated add may put a person in
	// directly.
	GroupLimit int
	// WritesPerMinute is how many delegated writes one caller may make in a
	// minute.
	WritesPerMinute int
	// TTL is how long reads that serve reads are kept: a caller's Groups, a
	// Group's role mappings, the realm's names, a managed tree, the
	// capabilities' managed Teams. FailureTTL keeps a failed managed Teams
	// read; Timeout bounds that read.
	TTL        time.Duration
	FailureTTL time.Duration
	Timeout    time.Duration
	Now        func() time.Time
	Logf       func(string, ...any)
}

// The variables that tune team delegation's limits (SPEC §4.5). Unset is
// the default; anything but a positive whole number stops startup.
const (
	DelegatedGroupLimitEnv = "TEAM_DELEGATION_GROUP_LIMIT"
	DelegatedWritesEnv     = "TEAM_DELEGATION_WRITES_PER_MINUTE"
)

// TeamDelegationOptionsFromEnv reads DelegatedGroupLimitEnv and
// DelegatedWritesEnv.
func TeamDelegationOptionsFromEnv(getenv func(string) string) (TeamDelegationOptions, error) {
	var out TeamDelegationOptions
	for _, v := range []struct {
		name string
		into *int
	}{{DelegatedGroupLimitEnv, &out.GroupLimit}, {DelegatedWritesEnv, &out.WritesPerMinute}} {
		raw := strings.TrimSpace(getenv(v.name))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return TeamDelegationOptions{}, fmt.Errorf("%s: %q is not a positive whole number", v.name, raw)
		}
		*v.into = n
	}
	return out, nil
}

// TeamDelegation reads what the delegated decisions need from Keycloak with
// core's service account (docs/team-delegation.md): the Leader groups a
// caller is directly in, a Group and its ancestors by id, and the role
// mappings that make a Group protected. Writes read the caller's Groups and
// the target fresh; reads keep the caller's Groups and the role mappings a
// minute, and core's own writes drop them.
type TeamDelegation struct {
	policy  authz.Policy
	dir     Directory
	nodes   GroupNodes
	options TeamDelegationOptions

	mu      sync.Mutex
	callers map[uuid.UUID]leaderEntry
	roles   map[string]protectedEntry
	names   namesEntry
	teams   map[uuid.UUID]teamsEntry
	trees   map[uuid.UUID]treeEntry
	writes  map[uuid.UUID]writeWindow
	reads   singleflight.Group
}

type leaderEntry struct {
	groups  []authz.LeaderGroup
	expires time.Time
}

type protectedEntry struct {
	protected bool
	expires   time.Time
}

type namesEntry struct {
	groups  []Group
	expires time.Time
}

type teamsEntry struct {
	teams   []ManagedTeam
	expires time.Time
}

type treeEntry struct {
	nodes      []ManagedNode
	privileged bool
	expires    time.Time
}

type writeWindow struct {
	start time.Time
	count int
}

// NewTeamDelegation decides by policy (its TeamDelegation switch, roots and
// leader subgroups) and reads dir and nodes.
func NewTeamDelegation(policy authz.Policy, dir Directory, nodes GroupNodes, options TeamDelegationOptions) *TeamDelegation {
	if options.GroupLimit <= 0 {
		options.GroupLimit = DefaultDelegatedGroupLimit
	}
	if options.WritesPerMinute <= 0 {
		options.WritesPerMinute = DefaultDelegatedWrites
	}
	if options.TTL <= 0 {
		options.TTL = defaultDelegationTTL
	}
	if options.FailureTTL <= 0 {
		options.FailureTTL = defaultDelegationFailTTL
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultDelegationTimeout
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logf == nil {
		options.Logf = func(string, ...any) {}
	}
	return &TeamDelegation{
		policy: policy, dir: dir, nodes: nodes, options: options,
		callers: map[uuid.UUID]leaderEntry{},
		roles:   map[string]protectedEntry{},
		teams:   map[uuid.UUID]teamsEntry{},
		trees:   map[uuid.UUID]treeEntry{},
		writes:  map[uuid.UUID]writeWindow{},
	}
}

// On reports whether team delegation is switched on.
func (d *TeamDelegation) On() bool { return d != nil && d.policy.TeamDelegation }

// Policy is the policy d decides by.
func (d *TeamDelegation) Policy() authz.Policy { return d.policy }

// Forget drops what d remembers of userID's Groups: core changed them.
// Every managed tree goes too, since its counts may have changed.
func (d *TeamDelegation) Forget(userID uuid.UUID) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.callers, userID)
	delete(d.teams, userID)
	clear(d.trees)
}

// ForgetAll drops everything d remembers: core renamed or remapped a Group.
func (d *TeamDelegation) ForgetAll() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	clear(d.callers)
	clear(d.roles)
	clear(d.teams)
	clear(d.trees)
	d.names = namesEntry{}
}

// allowWrite counts one delegated write of userID and reports whether it
// is within the minute's budget.
func (d *TeamDelegation) allowWrite(userID uuid.UUID) bool {
	now := d.options.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.writes[userID]
	if now.Sub(w.start) >= time.Minute || now.Before(w.start) {
		w = writeWindow{start: now}
	}
	if w.count >= d.options.WritesPerMinute {
		return false
	}
	w.count++
	d.writes[userID] = w
	if len(d.writes) > 4096 {
		for id, other := range d.writes {
			if now.Sub(other.start) >= time.Minute {
				delete(d.writes, id)
			}
		}
	}
	return true
}

// manager is the caller as a delegated decision reads them: the Leader
// groups they are directly in, read from Keycloak (fresh for a write, kept
// TTL for a read).
func (d *TeamDelegation) manager(ctx context.Context, userID uuid.UUID, fresh bool) (authz.TeamManager, error) {
	if !fresh {
		d.mu.Lock()
		entry, ok := d.callers[userID]
		d.mu.Unlock()
		if ok && d.options.Now().Before(entry.expires) {
			return authz.TeamManager{LeaderGroups: entry.groups}, nil
		}
	}
	groups, err := d.leaderGroups(ctx, userID)
	if err != nil {
		return authz.TeamManager{}, err
	}
	d.mu.Lock()
	d.callers[userID] = leaderEntry{groups: groups, expires: d.options.Now().Add(d.options.TTL)}
	d.mu.Unlock()
	return authz.TeamManager{LeaderGroups: groups}, nil
}

// leaderGroups are the Leader groups userID is directly in: of their
// Groups, those whose path names a Leader group, each read again by id for
// its name, path and parent.
func (d *TeamDelegation) leaderGroups(ctx context.Context, userID uuid.UUID) ([]authz.LeaderGroup, error) {
	groups, err := d.dir.GroupsForUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return []authz.LeaderGroup{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]authz.LeaderGroup, 0)
	for _, g := range groups {
		if _, role, ok := authz.TeamOf(g.Path, d.policy.PrivilegedRoots, d.policy.LeaderSubgroups); !ok || role == "" {
			continue
		}
		node, parent, err := d.nodes.GroupNode(ctx, g.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// The name itself must be a leader subgroup's: a name with a "/"
		// (X/LIDERLER) makes a path that only looks like one.
		if !slices.Contains(d.policy.LeaderSubgroups, node.Name) {
			continue
		}
		out = append(out, authz.LeaderGroup{ID: node.ID, Name: node.Name, Path: node.Path, TeamID: parent})
	}
	return out, nil
}

// target reads the Group id and every Group above it by id, and whether
// one of them maps a protected role. A chain whose ids and paths disagree
// is CodeGroupTreeMismatch, logged; an id Keycloak does not know is
// ErrNotFound.
func (d *TeamDelegation) target(ctx context.Context, id string) (authz.ManagedGroup, Group, error) {
	g, parent, err := d.nodes.GroupNode(ctx, id)
	if err != nil {
		return authz.ManagedGroup{}, Group{}, err
	}
	out := authz.ManagedGroup{Group: nodeOf(g, parent)}
	for next := parent; next != ""; {
		if len(out.Ancestors) == maxGroupDepth {
			return d.mismatch(out)
		}
		above, aboveParent, err := d.nodes.GroupNode(ctx, next)
		if errors.Is(err, ErrNotFound) {
			// Gone between two reads: the tree changed under us.
			return d.mismatch(out)
		}
		if err != nil {
			return authz.ManagedGroup{}, Group{}, err
		}
		out.Ancestors = append(out.Ancestors, nodeOf(above, aboveParent))
		next = aboveParent
	}
	if !authz.ChainHolds(out.Group, out.Ancestors) {
		return d.mismatch(out)
	}
	ids := []string{out.Group.ID}
	for _, a := range out.Ancestors {
		ids = append(ids, a.ID)
	}
	if out.Protected, err = d.protected(ctx, ids); err != nil {
		return authz.ManagedGroup{}, Group{}, err
	}
	return out, g, nil
}

// mismatch refuses g with CodeGroupTreeMismatch and logs it. g, as far as
// it was read, comes back with the error: the denied row names the Group.
func (d *TeamDelegation) mismatch(g authz.ManagedGroup) (authz.ManagedGroup, Group, error) {
	// Ids and the depth only: a name that broke the path may be anything.
	d.options.Logf("team delegation: group %s: ancestors by id and by path disagree (%d read)", g.Group.ID, len(g.Ancestors))
	return g, Group{}, refusedWith(http.StatusConflict, CodeGroupTreeMismatch)
}

func nodeOf(g Group, parent string) authz.GroupNode {
	return authz.GroupNode{ID: g.ID, Name: g.Name, Path: g.Path, ParentID: parent}
}

// protected reports whether one of the Groups ids maps a protected role
// (authz.ProtectedRole). Each Group's answer is kept TTL.
func (d *TeamDelegation) protected(ctx context.Context, ids []string) (bool, error) {
	for _, id := range ids {
		d.mu.Lock()
		entry, ok := d.roles[id]
		d.mu.Unlock()
		if !ok || !d.options.Now().Before(entry.expires) {
			roles, err := d.dir.GroupClientRoles(ctx, id)
			if err != nil {
				return false, err
			}
			entry = protectedEntry{expires: d.options.Now().Add(d.options.TTL)}
			for _, role := range roles {
				if authz.ProtectedRole(role.ClientID, role.Role) {
					entry.protected = true
					break
				}
			}
			d.mu.Lock()
			d.roles[id] = entry
			d.mu.Unlock()
		}
		if entry.protected {
			return true, nil
		}
	}
	return false, nil
}

// nameClash is the first name on the way to g (g and the Groups above it,
// Leader groups aside) that another Group of the realm also has, compared
// without case, or "". Owner team decisions read names from token paths,
// so a member added there would also count in the other Team. The realm's
// Groups are kept TTL: core's own creates and renames keep names unique,
// so only a change made in Keycloak's console is seen late.
func (d *TeamDelegation) nameClash(ctx context.Context, g authz.ManagedGroup) (string, error) {
	d.mu.Lock()
	entry := d.names
	d.mu.Unlock()
	if entry.groups == nil || !d.options.Now().Before(entry.expires) {
		groups, err := d.dir.ListGroups(ctx)
		if err != nil {
			return "", err
		}
		entry = namesEntry{groups: groups, expires: d.options.Now().Add(d.options.TTL)}
		d.mu.Lock()
		d.names = entry
		d.mu.Unlock()
	}
	for _, node := range append([]authz.GroupNode{g.Group}, g.Ancestors...) {
		if slices.Contains(d.policy.LeaderSubgroups, node.Name) {
			continue
		}
		for _, other := range entry.groups {
			if other.ID != node.ID && strings.EqualFold(strings.TrimSpace(other.Name), node.Name) {
				return node.Name, nil
			}
		}
	}
	return "", nil
}

// ManagedTeam is a Team the caller is a Leader of (capabilities'
// managedTeams).
type ManagedTeam struct {
	ID          string        `json:"id"`
	Path        string        `json:"path"`
	Code        string        `json:"code"`
	DisplayName LocalizedText `json:"displayName"`
}

// ManagedTeams are the Teams userID is directly in a Leader group of, read
// from Keycloak, in path order: empty with delegation off. An answer is kept
// TTL, one with a failure FailureTTL; a read waits Timeout at most, and a
// Team that cannot be read is left out with the error. Requests of one
// person share one read. The second result is whether the answer was
// remembered.
func (d *TeamDelegation) ManagedTeams(ctx context.Context, userID uuid.UUID) ([]ManagedTeam, bool, error) {
	if !d.On() {
		return []ManagedTeam{}, true, nil
	}
	d.mu.Lock()
	entry, ok := d.teams[userID]
	d.mu.Unlock()
	if ok && d.options.Now().Before(entry.expires) {
		return entry.teams, true, nil
	}
	type answer struct {
		teams []ManagedTeam
		err   error
	}
	v, _, _ := d.reads.Do("teams:"+userID.String(), func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.options.Timeout)
		defer cancel()
		teams, err := d.readManagedTeams(ctx, userID)
		ttl := d.options.TTL
		if err != nil {
			ttl = d.options.FailureTTL
		}
		d.mu.Lock()
		d.teams[userID] = teamsEntry{teams: teams, expires: d.options.Now().Add(ttl)}
		d.mu.Unlock()
		return answer{teams, err}, nil
	})
	got := v.(answer)
	return got.teams, false, got.err
}

func (d *TeamDelegation) readManagedTeams(ctx context.Context, userID uuid.UUID) ([]ManagedTeam, error) {
	leaders, err := d.leaderGroups(ctx, userID)
	if err != nil {
		return []ManagedTeam{}, err
	}
	out := make([]ManagedTeam, 0, len(leaders))
	seen := map[string]bool{}
	var failed error
	for _, leader := range leaders {
		if leader.TeamID == "" || seen[leader.TeamID] {
			continue
		}
		team, _, err := d.nodes.GroupNode(ctx, leader.TeamID)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				failed = err
			}
			continue
		}
		// The Leader group must be right under the Team it names, by id
		// and by path (authz.Policy.Manages reads it the same way).
		if leader.Path != team.Path+"/"+leader.Name || strings.Contains(team.Name, "/") {
			continue
		}
		if t, role, ok := authz.TeamOf(team.Path, d.policy.PrivilegedRoots, d.policy.LeaderSubgroups); !ok || role != "" || t != team.Path {
			continue
		}
		seen[team.ID] = true
		out = append(out, ManagedTeam{ID: team.ID, Path: team.Path, Code: team.Name, DisplayName: displayName(team)})
	}
	slices.SortFunc(out, func(a, b ManagedTeam) int { return strings.Compare(a.Path, b.Path) })
	return out, failed
}

// ManagedNode is a Team in a managed tree (GET /v1/users/me/managed-groups).
type ManagedNode struct {
	ID          string        `json:"id"`
	Code        string        `json:"code"`
	Path        string        `json:"path"`
	DisplayName LocalizedText `json:"displayName"`
	// MemberCount are the Team Group's direct members; LeaderCount the
	// people in its Leader groups, each once.
	MemberCount int `json:"memberCount"`
	LeaderCount int `json:"leaderCount"`
	// Protected is whether the Team or a Group above it maps a protected
	// role: only Privileged manages it.
	Protected bool `json:"protected"`
	// Can are the caller's decisions on the Team: reading it and its
	// members (group.read), changing its members (members.write) and its
	// Leaders' (leaders.write).
	Can      map[string]bool `json:"can"`
	Children []ManagedNode   `json:"children"`
}
