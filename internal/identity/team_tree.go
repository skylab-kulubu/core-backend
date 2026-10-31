package identity

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"golang.org/x/sync/singleflight"
)

var (
	_ authz.TeamTree     = (*TeamTree)(nil)
	_ authz.TeamSettings = (*TeamTree)(nil)
)

const (
	// DefaultTeamTreeTTL is how long core keeps the realm's Groups for the
	// Owner team decisions (ekip-yetkileri ticket 12).
	DefaultTeamTreeTTL = time.Minute
	// DefaultTeamTreeFailureTTL keeps a failed read, so a Keycloak that is
	// down is not asked on every decision.
	DefaultTeamTreeFailureTTL = 10 * time.Second
	// DefaultTeamTreeTimeout bounds one read of the realm's Groups.
	DefaultTeamTreeTimeout = 5 * time.Second
)

// GroupLister reads every Group of the realm with its path
// (Directory.ListGroups).
type GroupLister interface {
	ListGroups(ctx context.Context) ([]Group, error)
}

// TeamTreeOptions tune TeamTree. Zero values take the defaults.
type TeamTreeOptions struct {
	TTL        time.Duration
	FailureTTL time.Duration
	Timeout    time.Duration
	Now        func() time.Time
	Logf       func(string, ...any)
}

// TeamTree is authz.TeamTree over Keycloak: the realm's Groups, read with
// core's own service account and kept TTL, a failed read FailureTTL. A
// failed read answers no path and no Sub-team, so a decision falls back to
// the caller's own Teams. Decisions that arrive while the Groups are being
// read share that one read. A Group moved or renamed in Keycloak is seen
// when the minute is up.
type TeamTree struct {
	groups  GroupLister
	options TeamTreeOptions

	mu    sync.Mutex
	entry teamTreeEntry
	// generation counts ForgetAll: a read keeps its answer only if no
	// ForgetAll happened while it ran.
	generation uint64
	flights    singleflight.Group
}

type teamTreeEntry struct {
	groups  []Group
	read    bool
	expires time.Time
}

func NewTeamTree(groups GroupLister, options TeamTreeOptions) *TeamTree {
	if options.TTL <= 0 {
		options.TTL = DefaultTeamTreeTTL
	}
	if options.FailureTTL <= 0 {
		options.FailureTTL = DefaultTeamTreeFailureTTL
	}
	if options.Timeout <= 0 {
		options.Timeout = DefaultTeamTreeTimeout
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logf == nil {
		options.Logf = func(string, ...any) {}
	}
	return &TeamTree{groups: groups, options: options}
}

// TeamPath is the path of the one Group named team, compared without case
// and blanks: no answer when no Group or more than one has the name, or
// the Groups could not be read.
func (t *TeamTree) TeamPath(team string) (string, bool) {
	g, ok := t.only(team)
	if !ok {
		return "", false
	}
	return g.Path, true
}

// SubTeams are the names of the Groups below path, in the order Keycloak
// lists them.
func (t *TeamTree) SubTeams(path string) []string {
	groups, ok := t.read()
	if !ok {
		return nil
	}
	var out []string
	for _, g := range groups {
		if strings.HasPrefix(g.Path, path+"/") {
			out = append(out, g.Name)
		}
	}
	return out
}

// MembersManageCompetitors is the Team setting of the one Group named team
// (authz.TeamAttrMembersManageCompetitors): false only when it says
// "false"; true when it says anything else, or the Group cannot be found or
// the tree read (the default, today's rule).
func (t *TeamTree) MembersManageCompetitors(team string) bool {
	g, ok := t.only(team)
	if !ok {
		return true
	}
	return authz.MembersManageCompetitorsValue(g.Attributes[authz.TeamAttrMembersManageCompetitors])
}

// TeamCompetitionManage is the Team setting of the one Group named team
// (authz.TeamAttrCompetitionManage): true only when it says "true"; false
// when it says anything else, or the Group cannot be found or the tree read
// (the default: a widening setting fails closed; the read failure is
// logged by read).
func (t *TeamTree) TeamCompetitionManage(team string) bool {
	g, ok := t.only(team)
	if !ok {
		return false
	}
	return authz.CompetitionManageValue(g.Attributes[authz.TeamAttrCompetitionManage])
}

// ForgetAll drops the kept tree: core's own write of a Team's setting or
// name is seen at the next decision. A read already running when it is
// called may have read Keycloak before the write: its answer goes only to
// the decisions that were waiting for it, is not kept, and a decision after
// ForgetAll starts a read of its own.
func (t *TeamTree) ForgetAll() {
	t.mu.Lock()
	t.entry = teamTreeEntry{}
	t.generation++
	t.flights.Forget("tree")
	t.mu.Unlock()
}

// only is the one Group named team, compared without case and blanks.
func (t *TeamTree) only(team string) (Group, bool) {
	groups, ok := t.read()
	if !ok {
		return Group{}, false
	}
	team = strings.TrimSpace(team)
	var found Group
	n := 0
	for _, g := range groups {
		if strings.EqualFold(strings.TrimSpace(g.Name), team) {
			found = g
			n++
		}
	}
	return found, n == 1
}

func (t *TeamTree) read() ([]Group, bool) {
	t.mu.Lock()
	entry := t.entry
	t.mu.Unlock()
	if !entry.expires.IsZero() && t.options.Now().Before(entry.expires) {
		return entry.groups, entry.read
	}
	t.mu.Lock()
	generation := t.generation
	t.mu.Unlock()
	v, _, _ := t.flights.Do("tree", func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), t.options.Timeout)
		defer cancel()
		groups, err := t.groups.ListGroups(ctx)
		entry := teamTreeEntry{groups: groups, read: true, expires: t.options.Now().Add(t.options.TTL)}
		if err != nil {
			t.options.Logf("owner team tree: Keycloak's Groups could not be read; for %s Leaders lead only their own Teams and every Team has the default settings: %v", t.options.FailureTTL, err)
			entry = teamTreeEntry{expires: t.options.Now().Add(t.options.FailureTTL)}
		}
		t.mu.Lock()
		if t.generation == generation {
			t.entry = entry
		}
		t.mu.Unlock()
		return entry, nil
	})
	entry = v.(teamTreeEntry)
	return entry.groups, entry.read
}
