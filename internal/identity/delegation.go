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

// Protection is whether a Group's own role mappings make it a Protected
// group, and why (a role name, never a person): for the log.
type Protection struct {
	Protected bool
	Reason    string
	// Unclassified is the client role, "client:role", that protects the
	// Group because nobody has classified it (authz.RoleClassUnclassified),
	// "" otherwise: logged and counted so it gets classified.
	Unclassified string
}

// clientRoleProtection is the protection a client role gives on its own:
// none for an app role (authz.DelegableRole), else protected with its
// class.
func clientRoleProtection(clientID, role string) Protection {
	switch authz.ClassifyRole(clientID, role) {
	case authz.RoleClassApp:
		return Protection{}
	case authz.RoleClassAdmin:
		return Protection{Protected: true, Reason: "admin role " + clientID + ":" + role}
	default:
		return Protection{Protected: true, Reason: "unclassified client role " + clientID + ":" + role + " (counts as an admin role)", Unclassified: clientID + ":" + role}
	}
}

// GroupProtections reads a Group's own role mappings for protection: any
// realm role, or a client role that is no app role (authz.DelegableRole),
// or a composite app role that holds one or a realm role
// (Keycloak, Memory).
type GroupProtections interface {
	GroupProtection(ctx context.Context, groupID string) (Protection, error)
}

// TeamReads are the reads of team delegation beside the Directory's.
type TeamReads interface {
	GroupNodes
	GroupProtections
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
	// than a delegated caller may, or has one running.
	CodeRateLimited = "rate_limited"
	// CodeProtectionUnverified: core cannot see that a Privileged root is
	// protected, so it cannot trust that it sees any protection (a service
	// account that lost view-clients reads every role mapping as none,
	// without an error). Every delegated write is refused until it can.
	CodeProtectionUnverified = "protection_unverified"
)

// DelegationError is a delegated request refused with a problem code and
// the status it is answered with; RetryAfter, in seconds, when the caller
// may try again (0: no Retry-After).
type DelegationError struct {
	Status     int
	Code       string
	RetryAfter int
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
	// DefaultDelegatedWriteTimeout bounds a whole delegated write, lock
	// wait included.
	DefaultDelegatedWriteTimeout = 10 * time.Second
	// DefaultDelegatedSearches is how many candidate searches one caller
	// may make in a minute (review of #235, O2).
	DefaultDelegatedSearches = 30
	// maxGroupDepth bounds the walk up a Group's ancestors: a parent chain
	// longer than any real tree is a loop or a fault.
	maxGroupDepth = 32
	// The Team structure limits (SPEC §4.5, token size): how many levels of
	// Sub-teams a top Team may have below it, how long a Team's path may
	// be, and how many Sub-teams one Team may have.
	DefaultSubTeamDepth = 2
	DefaultTeamPathLen  = 128
	DefaultSubTeams     = 20
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
	// SearchesPerMinute is how many candidate searches one delegated
	// caller may make in a minute.
	SearchesPerMinute int
	// WriteTimeout bounds one delegated write, waiting for the lock
	// included.
	WriteTimeout time.Duration
	// SubTeamDepth, TeamPathLen and SubTeams are the Team structure
	// limits a create and a move keep to, for every caller
	// (DefaultSubTeamDepth, …).
	SubTeamDepth int
	TeamPathLen  int
	SubTeams     int
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
	SubTeamDepthEnv        = "TEAM_SUBTEAM_DEPTH_LIMIT"
	TeamPathLenEnv         = "TEAM_PATH_LENGTH_LIMIT"
	SubTeamsEnv            = "TEAM_SUBTEAMS_LIMIT"
)

// TeamDelegationOptionsFromEnv reads DelegatedGroupLimitEnv,
// DelegatedWritesEnv and the Team structure limits (SubTeamDepthEnv,
// TeamPathLenEnv, SubTeamsEnv).
func TeamDelegationOptionsFromEnv(getenv func(string) string) (TeamDelegationOptions, error) {
	var out TeamDelegationOptions
	for _, v := range []struct {
		name string
		into *int
	}{{DelegatedGroupLimitEnv, &out.GroupLimit}, {DelegatedWritesEnv, &out.WritesPerMinute},
		{SubTeamDepthEnv, &out.SubTeamDepth}, {TeamPathLenEnv, &out.TeamPathLen}, {SubTeamsEnv, &out.SubTeams}} {
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
	reads   TeamReads
	options TeamDelegationOptions

	mu      sync.Mutex
	callers map[uuid.UUID]leaderEntry
	roles   map[string]protectedEntry
	names   namesEntry
	teams   map[uuid.UUID]teamsEntry
	trees   map[uuid.UUID]treeEntry
	writes  map[uuid.UUID]writeWindow
	finds   map[uuid.UUID]writeWindow
	running map[uuid.UUID]bool
	// canaryUntil is when the last canary that passed stops counting.
	canaryUntil time.Time
	flights     singleflight.Group
	// unclassified counts the protection reads that found a client role
	// nobody has classified ("client:role", at most maxUnclassifiedLabels
	// of them, the rest as "other"): served on /v1/metrics.
	unclassified map[string]uint64
}

// maxUnclassifiedLabels bounds the role label of the unclassified role
// counter. Its values are Keycloak role names, never a person.
const maxUnclassifiedLabels = 64

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
	write      bool
	expires    time.Time
}

type writeWindow struct {
	start time.Time
	count int
}

// NewTeamDelegation decides by policy (its TeamDelegation switch, roots and
// leader subgroups) and reads dir and reads.
func NewTeamDelegation(policy authz.Policy, dir Directory, reads TeamReads, options TeamDelegationOptions) *TeamDelegation {
	if options.GroupLimit <= 0 {
		options.GroupLimit = DefaultDelegatedGroupLimit
	}
	if options.WritesPerMinute <= 0 {
		options.WritesPerMinute = DefaultDelegatedWrites
	}
	if options.SearchesPerMinute <= 0 {
		options.SearchesPerMinute = DefaultDelegatedSearches
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = DefaultDelegatedWriteTimeout
	}
	options.SubTeamDepth, options.TeamPathLen, options.SubTeams = structureLimits(options)
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
		policy: policy, dir: dir, reads: reads, options: options,
		callers: map[uuid.UUID]leaderEntry{},
		roles:   map[string]protectedEntry{},
		teams:   map[uuid.UUID]teamsEntry{},
		trees:   map[uuid.UUID]treeEntry{},
		writes:  map[uuid.UUID]writeWindow{},
		finds:   map[uuid.UUID]writeWindow{},
		running: map[uuid.UUID]bool{},

		unclassified: map[string]uint64{},
	}
}

// countUnclassified counts one protection read that found role, a
// "client:role" nobody has classified.
func (d *TeamDelegation) countUnclassified(role string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.unclassified[role]; !ok && len(d.unclassified) >= maxUnclassifiedLabels {
		role = "other"
	}
	d.unclassified[role]++
}

// Prometheus renders, per client role nobody has classified, how many
// protection reads found it (skylab_team_delegation_unclassified_roles_total
// {role="client:role"}). Such a role protects its Group as an admin role
// until it is classified (authz.AppRoles, authz.AdminRoles). Nobody is
// named. A nil TeamDelegation renders nothing.
func (d *TeamDelegation) Prometheus() string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	roles := make([]string, 0, len(d.unclassified))
	for role := range d.unclassified {
		roles = append(roles, role)
	}
	slices.Sort(roles)
	var out strings.Builder
	out.WriteString("# TYPE skylab_team_delegation_unclassified_roles_total counter\n")
	for _, role := range roles {
		fmt.Fprintf(&out, "skylab_team_delegation_unclassified_roles_total{role=%q} %d\n", role, d.unclassified[role])
	}
	d.mu.Unlock()
	return out.String()
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
	return d.spend(d.writes, userID, d.options.WritesPerMinute)
}

// allowSearch counts one candidate search of userID and reports whether it
// is within the minute's budget.
func (d *TeamDelegation) allowSearch(userID uuid.UUID) bool {
	return d.spend(d.finds, userID, d.options.SearchesPerMinute)
}

func (d *TeamDelegation) spend(windows map[uuid.UUID]writeWindow, userID uuid.UUID, budget int) bool {
	now := d.options.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	w := windows[userID]
	if now.Sub(w.start) >= time.Minute || now.Before(w.start) {
		w = writeWindow{start: now}
	}
	if w.count >= budget {
		return false
	}
	w.count++
	windows[userID] = w
	if len(windows) > 4096 {
		for id, other := range windows {
			if now.Sub(other.start) >= time.Minute {
				delete(windows, id)
			}
		}
	}
	return true
}

// beginWrite marks a delegated write of userID as running: false when one
// already is (one at a time per person, review of #235, B2). release ends
// it.
func (d *TeamDelegation) beginWrite(userID uuid.UUID) (release func(), ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running[userID] {
		return nil, false
	}
	d.running[userID] = true
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.running, userID)
	}, true
}

// Canary checks that core sees protection at all: a Privileged root that
// exists (its core role mappings, ADR-0059) must read as protected. Keycloak
// answers a caller without view-clients an empty role mapping, with no
// error, so without this check a service account that lost a permission
// would read every Group as unprotected and open them all (review of #235,
// O3). It fails closed: CodeProtectionUnverified (503), logged as an ERROR.
// A pass is kept TTL; a failure is checked again on the next write. The
// second result names the root that passed.
func (d *TeamDelegation) Canary(ctx context.Context) (string, error) {
	d.mu.Lock()
	passed := d.options.Now().Before(d.canaryUntil)
	d.mu.Unlock()
	if passed {
		return "", nil
	}
	var seen []string
	for _, root := range d.policy.PrivilegedRoots {
		g, err := d.dir.GetGroup(ctx, root)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		if g.Path != root || g.ID == "" {
			continue
		}
		seen = append(seen, root)
		got, err := d.reads.GroupProtection(ctx, g.ID)
		if err != nil {
			return "", err
		}
		if got.Protected {
			d.mu.Lock()
			d.canaryUntil = d.options.Now().Add(d.options.TTL)
			d.mu.Unlock()
			return root, nil
		}
	}
	d.options.Logf("ERROR team delegation: protection cannot be verified: no Privileged root reads as protected (found %d of %d: %s); is view-clients still held by core's service account? Every delegated write is refused (503 %s)",
		len(seen), len(d.policy.PrivilegedRoots), strings.Join(seen, ", "), CodeProtectionUnverified)
	return "", &DelegationError{Status: http.StatusServiceUnavailable, Code: CodeProtectionUnverified, RetryAfter: 60}
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
		node, parent, err := d.reads.GroupNode(ctx, g.ID)
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
// one of them is protected (read fresh for a write, kept TTL for a read).
// A chain whose ids and paths disagree is CodeGroupTreeMismatch, logged;
// an id Keycloak does not know is ErrNotFound.
func (d *TeamDelegation) target(ctx context.Context, id string, fresh bool) (authz.ManagedGroup, Group, error) {
	g, parent, err := d.reads.GroupNode(ctx, id)
	if err != nil {
		return authz.ManagedGroup{}, Group{}, err
	}
	out := authz.ManagedGroup{Group: nodeOf(g, parent)}
	for next := parent; next != ""; {
		if len(out.Ancestors) == maxGroupDepth {
			return d.mismatch(out)
		}
		above, aboveParent, err := d.reads.GroupNode(ctx, next)
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
	if out.Protected, err = d.protected(ctx, ids, fresh); err != nil {
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

// protected reports whether one of the Groups ids is protected by its own
// role mappings (GroupProtections): any realm role, or a protected client
// role directly or inside a composite. A write reads each fresh (review of
// #235, O1); a read keeps each Group's answer TTL. A protected Group is
// logged with its reason: a realm role mapped high in the tree (on /UYELER,
// on an area) makes every Team under it Privileged's alone.
func (d *TeamDelegation) protected(ctx context.Context, ids []string, fresh bool) (bool, error) {
	for _, id := range ids {
		d.mu.Lock()
		entry, ok := d.roles[id]
		d.mu.Unlock()
		if fresh || !ok || !d.options.Now().Before(entry.expires) {
			got, err := d.reads.GroupProtection(ctx, id)
			if err != nil {
				return false, err
			}
			entry = protectedEntry{protected: got.Protected, expires: d.options.Now().Add(d.options.TTL)}
			d.mu.Lock()
			d.roles[id] = entry
			d.mu.Unlock()
			if got.Protected {
				d.options.Logf("team delegation: group %s is protected (%s): it and every Group under it are managed by Privileged only", id, got.Reason)
			}
			if got.Unclassified != "" {
				d.countUnclassified(got.Unclassified)
				d.options.Logf("team delegation: client role %s is unclassified and counts as an admin role; classify it in authz.AppRoles or authz.AdminRoles (docs/team-delegation.md)", got.Unclassified)
			}
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
			if other.ID != node.ID && strings.EqualFold(strings.TrimSpace(other.Name), strings.TrimSpace(node.Name)) {
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
	v, _, _ := d.flights.Do("teams:"+userID.String(), func() (any, error) {
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
		team, _, err := d.reads.GroupNode(ctx, leader.TeamID)
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

// structureLimits are options' Team structure limits, a default for each
// one unset.
func structureLimits(options TeamDelegationOptions) (depth, pathLen, subTeams int) {
	depth, pathLen, subTeams = options.SubTeamDepth, options.TeamPathLen, options.SubTeams
	if depth <= 0 {
		depth = DefaultSubTeamDepth
	}
	if pathLen <= 0 {
		pathLen = DefaultTeamPathLen
	}
	if subTeams <= 0 {
		subTeams = DefaultSubTeams
	}
	return depth, pathLen, subTeams
}
