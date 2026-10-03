package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

// ErrGroupsUnavailable is a person's Groups that could not be read from
// Keycloak. It never means the person has none.
var ErrGroupsUnavailable = errors.New("identity: groups unavailable")

const (
	// DefaultOverageTTL is how long core keeps a person's Groups read for a
	// Group overage token (ADR-0059).
	DefaultOverageTTL = time.Minute
	// DefaultOverageTimeout bounds one read of a person's Groups.
	DefaultOverageTimeout = 5 * time.Second
	// overageSweepSize is the number of remembered people at which a new
	// answer first drops the expired ones.
	overageSweepSize = 1024
)

// GroupReader reads a person's direct Group memberships with their full
// paths: Keycloak's GET /users/{id}/groups (Directory.GroupsForUser), the
// paths the Group Membership mapper would have put into the token.
type GroupReader interface {
	GroupsForUser(ctx context.Context, userID uuid.UUID) ([]Group, error)
}

// GroupCache is what core remembers of people's Groups. The identity service
// forgets a person there whenever it writes their memberships, and everyone
// when it renames a Group, which changes every path under it.
type GroupCache interface {
	Forget(userID uuid.UUID)
	ForgetAll()
}

// OverageOptions tune OverageGroups. Zero values take the defaults.
type OverageOptions struct {
	TTL     time.Duration
	Timeout time.Duration
	Now     func() time.Time
}

// OverageGroups answers the Group paths of a person whose access token
// carries the Group overage marker instead of the groups claim (ADR-0059):
// core reads them from Keycloak with its own service account, the way an
// Entra application asks Microsoft Graph.
//
// An answer is kept for a minute per person (`sub`). The token's own claims
// do not take part in the key: the person's Groups do not depend on which
// token asks, and the minute already bounds how old an answer can be.
// Requests of one person that arrive while their Groups are being read
// share that one read. A failed read is never kept and never answers an
// empty list: it is ErrGroupsUnavailable, or ErrNotFound when Keycloak does
// not know the person.
//
// Forget and ForgetAll drop answers at once, and an answer read before one
// of them is not kept. The answers live in this process: a write through
// another core process reaches this one when its minute is up.
type OverageGroups struct {
	reader  GroupReader
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	entries map[uuid.UUID]overageEntry
	// generation counts Forget and ForgetAll. A read keeps its answer only
	// if no forget happened while it ran, and a request after a forget
	// does not join a read that started before it.
	generation uint64
	reads      singleflight.Group

	requests atomic.Uint64
	hits     atomic.Uint64
	lookups  atomic.Uint64
	failures atomic.Uint64
	unknown  atomic.Uint64
}

type overageEntry struct {
	paths   []string
	expires time.Time
}

func NewOverageGroups(reader GroupReader, options OverageOptions) *OverageGroups {
	o := &OverageGroups{
		reader:  reader,
		ttl:     options.TTL,
		timeout: options.Timeout,
		now:     options.Now,
		entries: map[uuid.UUID]overageEntry{},
	}
	if o.ttl <= 0 {
		o.ttl = DefaultOverageTTL
	}
	if o.timeout <= 0 {
		o.timeout = DefaultOverageTimeout
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// Paths answers the person's Group paths and whether the answer was
// remembered rather than read now.
func (o *OverageGroups) Paths(ctx context.Context, userID uuid.UUID) ([]string, bool, error) {
	if o == nil || o.reader == nil {
		return nil, false, ErrGroupsUnavailable
	}
	o.requests.Add(1)
	o.mu.Lock()
	if entry, ok := o.entries[userID]; ok && o.now().Before(entry.expires) {
		o.mu.Unlock()
		o.hits.Add(1)
		return slices.Clone(entry.paths), true, nil
	}
	generation := o.generation
	o.mu.Unlock()

	read := o.reads.DoChan(userID.String()+"@"+strconv.FormatUint(generation, 10), func() (any, error) {
		return o.read(userID, generation)
	})
	// The read has its own deadline, and the Keycloak client lets a call
	// waiting for the service account's token leave when its context ends.
	// The request still bounds its own wait, so it never outlasts o.timeout
	// whatever the reader does.
	wait := time.NewTimer(o.timeout)
	defer wait.Stop()
	select {
	case result := <-read:
		if result.Err != nil {
			return nil, false, result.Err
		}
		return slices.Clone(result.Val.([]string)), false, nil
	case <-wait.C:
		return nil, false, fmt.Errorf("%w: no answer within %s", ErrGroupsUnavailable, o.timeout)
	case <-ctx.Done():
		return nil, false, fmt.Errorf("%w: %v", ErrGroupsUnavailable, ctx.Err())
	}
}

// read asks Keycloak once for everyone waiting. It is not bound to the
// request that started it, which may leave, only to its own timeout.
func (o *OverageGroups) read(userID uuid.UUID, generation uint64) (paths []string, err error) {
	// singleflight runs this in a goroutine of its own and, should it panic,
	// panics again there, which ends the whole process. The callers get the
	// answer they get when Keycloak cannot be reached.
	defer func() {
		if r := recover(); r != nil {
			o.failures.Add(1)
			paths, err = nil, fmt.Errorf("%w: the read panicked: %v", ErrGroupsUnavailable, r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	o.lookups.Add(1)
	groups, err := o.reader.GroupsForUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		o.unknown.Add(1)
		return nil, ErrNotFound
	}
	if err != nil {
		o.failures.Add(1)
		// GroupsForUser's errors name nobody.
		return nil, fmt.Errorf("%w: %v", ErrGroupsUnavailable, err)
	}
	paths = GroupPaths(groups)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.generation == generation {
		if len(o.entries) >= overageSweepSize {
			o.sweepLocked()
		}
		o.entries[userID] = overageEntry{paths: paths, expires: o.now().Add(o.ttl)}
	}
	return paths, nil
}

func (o *OverageGroups) sweepLocked() {
	now := o.now()
	for id, entry := range o.entries {
		if !now.Before(entry.expires) {
			delete(o.entries, id)
		}
	}
}

// Forget drops what is remembered of the person's Groups.
func (o *OverageGroups) Forget(userID uuid.UUID) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.entries, userID)
	o.generation++
}

// ForgetAll drops what is remembered of everyone's Groups.
func (o *OverageGroups) ForgetAll() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	clear(o.entries)
	o.generation++
}

// Prometheus renders fixed, unlabelled counters and one gauge: the number of
// people whose Groups are remembered now, which is how many people with a
// Group overage token used core in the last minute. Nobody is named.
func (o *OverageGroups) Prometheus() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	now := o.now()
	people := 0
	for _, entry := range o.entries {
		if now.Before(entry.expires) {
			people++
		}
	}
	o.mu.Unlock()
	values := []struct {
		name  string
		value uint64
	}{
		{"skylab_group_overage_requests_total", o.requests.Load()},
		{"skylab_group_overage_cache_hits_total", o.hits.Load()},
		{"skylab_group_overage_lookups_total", o.lookups.Load()},
		{"skylab_group_overage_lookup_failures_total", o.failures.Load()},
		{"skylab_group_overage_unknown_subjects_total", o.unknown.Load()},
		{"skylab_group_overage_people", uint64(people)},
	}
	var out strings.Builder
	for _, metric := range values {
		out.WriteString(metric.name)
		out.WriteByte(' ')
		out.WriteString(strconv.FormatUint(metric.value, 10))
		out.WriteByte('\n')
	}
	return out.String()
}
