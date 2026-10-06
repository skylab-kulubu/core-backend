// Package editablesites answers which Site clients a person may edit in the
// CMS, for the admin panel's site list ("editableSites" in
// GET /v1/users/me/capabilities). The list is a hint for the UI, never an
// authorization: the CMS (inscribed) decides every write from the person's own
// token. Contract: docs/authz-roles.md.
package editablesites

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"golang.org/x/sync/singleflight"
)

// AccessRole is the client role on a Site client that makes its holder a
// CMS editor of that site (CONTEXT "cms:access", "Site editor").
const AccessRole = "cms:access"

// ErrUnavailable is a person's roles on one or more sites that could not be
// read from Keycloak. Those sites are left out of the answer; it never means
// the person may not edit them.
var ErrUnavailable = errors.New("editablesites: roles unavailable")

const (
	// DefaultTTL is how long one person's answer is kept.
	DefaultTTL = time.Minute
	// DefaultFailureTTL is how long an answer with a failure is kept, so
	// that Keycloak is not asked on every request while it is down.
	DefaultFailureTTL = 10 * time.Second
	// DefaultTimeout bounds one read of a person's sites, every site
	// together. The capabilities answer waits no longer.
	DefaultTimeout = 3 * time.Second
	// sweepSize is the number of remembered people at which a new answer
	// first drops the expired ones.
	sweepSize = 1024
)

// RoleReader reads the names of the roles of a Keycloak client a person holds
// in effect (identity.Keycloak.EffectiveClientRoles). A client or person
// Keycloak does not know is identity.ErrNotFound.
type RoleReader interface {
	EffectiveClientRoles(ctx context.Context, userID uuid.UUID, clientID string) ([]string, error)
}

// Options tune Sites. Zero values take the defaults.
type Options struct {
	TTL        time.Duration
	FailureTTL time.Duration
	Timeout    time.Duration
	Now        func() time.Time
}

// Sites answers the configured sites a person holds cms:access on, read with
// core's own service account from Keycloak: the same effective client roles
// a token for that site carries, so the same rule as the CMS (directly,
// through the person's Groups and the Groups above them, through composite
// roles). Nothing is read from the caller's token.
//
// An answer is kept a minute per person (`sub`); one with a failure is kept
// ten seconds. Requests of one person that arrive while their sites are being
// read share that read. A site whose roles cannot be read is left out and the
// answer carries ErrUnavailable; a site client the realm does not have is
// left out quietly.
type Sites struct {
	sites      []Site
	reader     RoleReader
	ttl        time.Duration
	failureTTL time.Duration
	timeout    time.Duration
	now        func() time.Time

	mu      sync.Mutex
	entries map[uuid.UUID]entry
	reads   singleflight.Group
}

type entry struct {
	sites   []Site
	expires time.Time
}

type answer struct {
	sites []Site
	err   error
}

// New answers from sites, in their order, with reader. No site or no reader
// answers every person an empty list.
func New(sites []Site, reader RoleReader, options Options) *Sites {
	s := &Sites{
		sites:      slices.Clone(sites),
		reader:     reader,
		ttl:        options.TTL,
		failureTTL: options.FailureTTL,
		timeout:    options.Timeout,
		now:        options.Now,
		entries:    map[uuid.UUID]entry{},
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	if s.failureTTL <= 0 {
		s.failureTTL = DefaultFailureTTL
	}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// For answers the sites the person may edit, never nil, and whether the
// answer was remembered rather than read now. err (ErrUnavailable) says that
// some sites could not be read and are missing; the list is still the answer
// to give. It names nobody.
func (s *Sites) For(ctx context.Context, userID uuid.UUID) (sites []Site, cached bool, err error) {
	if s == nil || s.reader == nil || len(s.sites) == 0 {
		return []Site{}, false, nil
	}
	s.mu.Lock()
	if e, ok := s.entries[userID]; ok && s.now().Before(e.expires) {
		s.mu.Unlock()
		return slices.Clone(e.sites), true, nil
	}
	s.mu.Unlock()

	read := s.reads.DoChan(userID.String(), func() (any, error) {
		return s.read(userID), nil
	})
	wait := time.NewTimer(s.timeout)
	defer wait.Stop()
	select {
	case result := <-read:
		a := result.Val.(answer)
		return slices.Clone(a.sites), false, a.err
	case <-wait.C:
		return []Site{}, false, fmt.Errorf("%w: no answer within %s", ErrUnavailable, s.timeout)
	case <-ctx.Done():
		return []Site{}, false, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
	}
}

// read asks Keycloak about every site at once, for everyone waiting. It is
// not bound to the request that started it, only to its own timeout.
func (s *Sites) read(userID uuid.UUID) answer {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	editable := make([]bool, len(s.sites))
	failures := make([]error, len(s.sites))
	var wg sync.WaitGroup
	for i, site := range s.sites {
		wg.Add(1)
		go func() {
			defer wg.Done()
			editable[i], failures[i] = s.holds(ctx, userID, site.ClientID)
		}()
	}
	wg.Wait()

	out := []Site{}
	failed := 0
	var first error
	for i, site := range s.sites {
		if failures[i] != nil {
			failed++
			if first == nil {
				first = failures[i]
			}
			continue
		}
		if editable[i] {
			out = append(out, site)
		}
	}
	a := answer{sites: out}
	ttl := s.ttl
	if failed > 0 {
		// The reader's errors name nobody (identity.Keycloak).
		a.err = fmt.Errorf("%w: %d of %d sites: %v", ErrUnavailable, failed, len(s.sites), first)
		ttl = s.failureTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= sweepSize {
		now := s.now()
		for id, e := range s.entries {
			if !now.Before(e.expires) {
				delete(s.entries, id)
			}
		}
	}
	s.entries[userID] = entry{sites: out, expires: s.now().Add(ttl)}
	return a
}

// holds says whether the person holds cms:access on the client. A client or
// person Keycloak does not know holds nothing.
func (s *Sites) holds(ctx context.Context, userID uuid.UUID, clientID string) (ok bool, err error) {
	// A panic here would end the whole process: it runs in a goroutine of
	// its own.
	defer func() {
		if r := recover(); r != nil {
			ok, err = false, fmt.Errorf("the read panicked: %v", r)
		}
	}()
	roles, err := s.reader.EffectiveClientRoles(ctx, userID, clientID)
	if errors.Is(err, identity.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.Contains(roles, AccessRole), nil
}
