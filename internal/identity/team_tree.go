package identity

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"golang.org/x/sync/singleflight"
)

var _ authz.TeamTree = (*TeamTree)(nil)

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

	mu      sync.Mutex
	entry   teamTreeEntry
	flights singleflight.Group
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
	groups, ok := t.read()
	if !ok {
		return "", false
	}
	team = strings.TrimSpace(team)
	path, found := "", 0
	for _, g := range groups {
		if strings.EqualFold(strings.TrimSpace(g.Name), team) {
			path = g.Path
			found++
		}
	}
	if found != 1 {
		return "", false
	}
	return path, true
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

func (t *TeamTree) read() ([]Group, bool) {
	t.mu.Lock()
	entry := t.entry
	t.mu.Unlock()
	if !entry.expires.IsZero() && t.options.Now().Before(entry.expires) {
		return entry.groups, entry.read
	}
	v, _, _ := t.flights.Do("tree", func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), t.options.Timeout)
		defer cancel()
		groups, err := t.groups.ListGroups(ctx)
		entry := teamTreeEntry{groups: groups, read: true, expires: t.options.Now().Add(t.options.TTL)}
		if err != nil {
			t.options.Logf("owner team tree: Keycloak's Groups could not be read, Leaders lead only their own Teams for %s: %v", t.options.FailureTTL, err)
			entry = teamTreeEntry{expires: t.options.Now().Add(t.options.FailureTTL)}
		}
		t.mu.Lock()
		t.entry = entry
		t.mu.Unlock()
		return entry, nil
	})
	entry = v.(teamTreeEntry)
	return entry.groups, entry.read
}
