package identity

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"golang.org/x/sync/singleflight"
)

// The Forms seat search (GET /v1/users?role=skyforms:…&q=, Forms' people
// picker; ekip-yetkileri ticket 04, post-cutover 17). Its reads are kept a
// minute (seatCacheTTL): the seat's holders through Groups, each person's
// effective seat and each person's membership. A membership or role
// mapping written through core drops them at once; one made in Keycloak's
// console shows within the minute.

// seatCacheTTL is how long the seat search keeps what it read.
const seatCacheTTL = time.Minute

// seatCacheEntries bounds the per-person entries the seat search keeps; past
// it the expired ones are dropped first, then all.
const seatCacheEntries = 8192

// seatGroupLimit bounds the Groups one read of the seat's holders walks
// (the mapped Groups and every Group under them): past it the rest are
// left out and core logs it.
const seatGroupLimit = 1000

// memberSeatSearch limits: a member who may not read people (no users:read)
// searches with at least three letters, gets at most ten people and makes
// at most thirty searches a minute, as a Leader's candidate search
// (SPEC §6.2).
const (
	memberSeatQueryMin = candidateQueryMin
	memberSeatLimit    = candidateLimit
	memberSeatSearches = DefaultDelegatedSearches
)

// MemberSeatSearchEnv switches the members' seat search: "on" or unset is
// on (the default), "off" answers a member without users:read 403 as
// before.
const MemberSeatSearchEnv = "SEAT_SEARCH_MEMBERS"

// ParseMemberSeatSearch reads MemberSeatSearchEnv: empty or "on" is on,
// "off" is off; anything else is an error that names the variable.
func ParseMemberSeatSearch(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s: %q is not %q or %q", MemberSeatSearchEnv, raw, "on", "off")
	}
}

// MemberSeatSearchFromEnv is ParseMemberSeatSearch of the process's variable.
func MemberSeatSearchFromEnv() (bool, error) {
	return ParseMemberSeatSearch(os.Getenv(MemberSeatSearchEnv))
}

// seatHolder is one person who holds the seat through a direct mapping:
// on themselves or on a Group they are in, or one above theirs. Member is
// whether the Group says they are a member (memberPath, or a role group
// under /ROLLER: a seat given on purpose); a person who holds it on
// themselves has no Group to say so.
type seatHolder struct {
	Person
	member bool
}

type cachedHolders struct {
	at     time.Time
	people []seatHolder
}

type cachedFlag struct {
	at time.Time
	ok bool
}

type seatFlagKey struct {
	key string
	id  uuid.UUID
}

type searchWindow struct {
	start time.Time
	count int
}

// seatCache is what the seat search keeps (seatCacheTTL) and the members'
// search budget.
type seatCache struct {
	now    func() time.Time
	flight singleflight.Group

	mu         sync.Mutex
	generation uint64
	holders    map[string]cachedHolders
	holds      map[seatFlagKey]cachedFlag
	members    map[uuid.UUID]cachedFlag
	searches   map[string]searchWindow
}

func newSeatCache(now func() time.Time) *seatCache {
	if now == nil {
		now = time.Now
	}
	return &seatCache{
		now:      now,
		holders:  map[string]cachedHolders{},
		holds:    map[seatFlagKey]cachedFlag{},
		members:  map[uuid.UUID]cachedFlag{},
		searches: map[string]searchWindow{},
	}
}

func seatKey(clientID, role string) string { return clientID + "\x00" + role }

// forget drops every read kept: a membership or a role mapping changed.
func (c *seatCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	clear(c.holders)
	clear(c.holds)
	clear(c.members)
}

func (c *seatCache) fresh(at time.Time) bool {
	now := c.now()
	return !now.Before(at) && now.Sub(at) < seatCacheTTL
}

// holdersOf answers the seat's holders kept, or reads them with read (one
// read at a time per seat); a failed read is not kept.
func (c *seatCache) holdersOf(key string, read func() ([]seatHolder, error)) ([]seatHolder, error) {
	c.mu.Lock()
	if e, ok := c.holders[key]; ok && c.fresh(e.at) {
		c.mu.Unlock()
		return e.people, nil
	}
	c.mu.Unlock()
	v, err, _ := c.flight.Do(key, func() (any, error) {
		c.mu.Lock()
		generation := c.generation
		c.mu.Unlock()
		at := c.now()
		people, err := read()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.generation == generation {
			c.holders[key] = cachedHolders{at: at, people: people}
		}
		c.mu.Unlock()
		return people, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]seatHolder), nil
}

// cachedRead answers a per-person answer kept in m, or reads it with read; a
// failed read is not kept.
func cachedRead[K comparable](c *seatCache, m map[K]cachedFlag, k K, read func() (bool, error)) (bool, error) {
	c.mu.Lock()
	if e, ok := m[k]; ok && c.fresh(e.at) {
		c.mu.Unlock()
		return e.ok, nil
	}
	generation := c.generation
	c.mu.Unlock()
	at := c.now()
	ok, err := read()
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		return ok, nil
	}
	if len(m) >= seatCacheEntries {
		for key, e := range m {
			if !c.fresh(e.at) {
				delete(m, key)
			}
		}
		if len(m) >= seatCacheEntries {
			clear(m)
		}
	}
	m[k] = cachedFlag{at: at, ok: ok}
	return ok, nil
}

// allowSearch counts one members' seat search of caller and reports
// whether it is within the minute's budget.
func (c *seatCache) allowSearch(caller string) bool {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.searches) >= seatCacheEntries {
		for key, w := range c.searches {
			if now.Sub(w.start) >= time.Minute || now.Before(w.start) {
				delete(c.searches, key)
			}
		}
	}
	w := c.searches[caller]
	if now.Sub(w.start) >= time.Minute || now.Before(w.start) {
		w = searchWindow{start: now}
	}
	if w.count >= memberSeatSearches {
		c.searches[caller] = w
		return false
	}
	w.count++
	c.searches[caller] = w
	return true
}

// seatHolders reads the people who hold clientID's role through a direct
// mapping: on themselves, on a Group they are in or on a Group above
// theirs (Keycloak gives a Group's roles to the members of every Group
// under it). People Keycloak has disabled get no token and are left out,
// so are the erased and those being erased (overlayShadow, which also puts
// core's profile over Keycloak's). Kept seatCacheTTL.
func (s *service) seatHolders(ctx context.Context, clientID, role string) ([]seatHolder, error) {
	return s.seats.holdersOf(seatKey(clientID, role), func() ([]seatHolder, error) {
		groups, direct, err := s.dir.ClientRoleHolders(ctx, clientID, role)
		if err != nil {
			return nil, err
		}
		people := make([]Person, 0, len(direct))
		member := map[uuid.UUID]bool{}
		seen := map[uuid.UUID]struct{}{}
		add := func(person Person, via string) {
			if person.ID == uuid.Nil || !person.Enabled {
				return
			}
			if via != "" && (s.memberPath(via) || strings.HasPrefix(via, authz.RolesRoot+"/")) {
				member[person.ID] = true
			}
			if _, ok := seen[person.ID]; ok {
				return
			}
			seen[person.ID] = struct{}{}
			people = append(people, person)
		}
		for _, person := range direct {
			add(person, "")
		}
		queue := append([]Group(nil), groups...)
		walked := map[string]struct{}{}
		for len(queue) > 0 {
			g := queue[0]
			queue = queue[1:]
			if _, ok := walked[g.ID]; ok || g.ID == "" {
				continue
			}
			if len(walked) >= seatGroupLimit {
				log.Printf("identity: seat search: %s:%s is mapped to more than %d Groups and the Groups under them; the rest are found by search only", clientID, role, seatGroupLimit)
				break
			}
			walked[g.ID] = struct{}{}
			members, err := s.dir.Members(ctx, g.ID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, person := range members {
				add(person, g.Path)
			}
			kids, err := s.dir.Subgroups(ctx, g.ID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			queue = append(queue, kids...)
		}
		people, err = s.overlayShadow(ctx, people)
		if err != nil {
			return nil, err
		}
		out := make([]seatHolder, 0, len(people))
		for _, person := range people {
			out = append(out, seatHolder{Person: person, member: member[person.ID]})
		}
		return out, nil
	})
}

// holdsSeatInEffect reads whether the person holds clientID's role in
// effect (Keycloak's effective client roles: Groups, the default roles and
// composites included). ErrClientNotFound: the realm has no clientID. Kept
// seatCacheTTL.
func (s *service) holdsSeatInEffect(ctx context.Context, clientID, role string, id uuid.UUID) (bool, error) {
	return cachedRead(s.seats, s.seats.holds, seatFlagKey{key: seatKey(clientID, role), id: id}, func() (bool, error) {
		names, err := s.dir.EffectiveClientRoles(ctx, id, clientID)
		if errors.Is(err, ErrClientNotFound) {
			return false, err
		}
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return holdsSeat(names, role), nil
	})
}

// isMember reads whether the person is a member (memberPath) through any
// Group they are in. Kept seatCacheTTL.
func (s *service) isMember(ctx context.Context, id uuid.UUID) (bool, error) {
	return cachedRead(s.seats, s.seats.members, id, func() (bool, error) {
		groups, err := s.dir.GroupsForUser(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(groups, func(g Group) bool { return s.memberPath(g.Path) }), nil
	})
}

// memberPath reports whether a Group at path makes its people members of
// the club for the members' seat search (ekip-yetkileri ticket 04): a Team,
// a Sub-team or a Leader group (/UYELER/<area>/<team>…), or a Privileged
// root or a Group under one. /UYELER itself, where an account that signed
// itself up lands, an area (/UYELER/ARGE), /ROLLER and anything else are
// not: such an account is like Entra's guest and searches nobody.
func (s *service) memberPath(path string) bool {
	if authz.PrivilegedRoot(s.privilegedRoots, path) != "" {
		return true
	}
	_, _, ok := authz.TeamOf(path, s.privilegedRoots, authz.DefaultPolicy().LeaderSubgroups)
	return ok
}

// memberCaller reports whether p is a person (not a product's or a
// client's service account) whose token names a member's Group
// (memberPath). A token without its Groups (no groups claim, or an overage
// core could not read) is no member: closed.
func (s *service) memberCaller(p authz.Principal) bool {
	if p.Product != "" || p.ServiceAccount || p.ID == "" {
		return false
	}
	return slices.ContainsFunc(p.Groups, s.memberPath)
}

// seatCaller reports whether p may make the members' seat search: a member
// (memberCaller), or a person (not a product's or a client's service
// account) who holds the seat in effect, e.g. through /ROLLER/FORMS-ACCESS
// only (ekip-yetkileri ticket 16). The seat is read as holdsSeatInEffect
// reads it, kept seatCacheTTL; a realm without the client is no holder.
func (s *service) seatCaller(ctx context.Context, p authz.Principal, clientID, role string) (bool, error) {
	if s.memberCaller(p) {
		return true, nil
	}
	if p.Product != "" || p.ServiceAccount {
		return false, nil
	}
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return false, nil
	}
	holds, err := s.holdsSeatInEffect(ctx, clientID, role, id)
	if errors.Is(err, ErrClientNotFound) {
		return false, nil
	}
	return holds, err
}

// memberSeatSearch is the seat search of a member who may not read people
// (ekip-yetkileri ticket 04), or of a person who holds the seat
// (seatCaller, ticket 16): Forms' seat (skyforms:access) only, a query of
// at least three letters (shorter answers no one), thirty searches a
// minute. It answers at most ten people who hold the seat and are members
// (or hold it through a role group), by the names only, e-mail masked; a
// query that is a whole address answers only that address's holder,
// unmasked, as the candidate search does (MemberCandidates).
func (s *service) memberSeatSearch(ctx context.Context, p authz.Principal, clientID, role, q string) ([]Person, error) {
	if !s.memberSeats || clientID != "forms" || role != "skyforms:access" {
		return nil, ErrForbidden
	}
	ok, err := s.seatCaller(ctx, p, clientID, role)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrForbidden
	}
	if utf8.RuneCountInString(q) < memberSeatQueryMin {
		return []Person{}, nil
	}
	if !s.seats.allowSearch(p.ID) {
		return nil, &DelegationError{Status: http.StatusTooManyRequests, Code: CodeRateLimited, RetryAfter: 60}
	}
	address := strings.Contains(q, "@")
	matches := func(person Person) bool {
		if address {
			return strings.EqualFold(strings.TrimSpace(person.Email), q)
		}
		return namesMatch(person, q)
	}
	people, err := s.seatSearch(ctx, clientID, role, q, matches, true)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(people, func(a, b Person) int {
		return strings.Compare(foldSearch(a.LastName+"\x00"+a.FirstName+"\x00"+a.Email), foldSearch(b.LastName+"\x00"+b.FirstName+"\x00"+b.Email))
	})
	if len(people) > memberSeatLimit {
		people = people[:memberSeatLimit]
	}
	out := projectPeople(people, false)
	if !address {
		for i := range out {
			out[i].Email = MaskEmail(out[i].Email)
		}
	}
	return out, nil
}

// namesMatch reports whether the query is part of the person's first name,
// last name or both, folded (foldSearch).
func namesMatch(person Person, q string) bool {
	needle := foldSearch(strings.TrimSpace(q))
	for _, field := range []string{person.FirstName, person.LastName, strings.TrimSpace(person.FirstName + " " + person.LastName)} {
		if strings.Contains(foldSearch(field), needle) {
			return true
		}
	}
	return false
}

// searchFold maps the letters a Turkish name is written with to the
// letter they are typed as without a Turkish keyboard: ı, I, İ and i all
// match each other, so do ş and s, ğ and g, ü and u, ö and o, ç and c, and
// the circumflexed vowels and their plain ones.
var searchFold = map[rune]rune{
	'ı': 'i', 'İ': 'i', 'I': 'i', 'î': 'i', 'Î': 'i',
	'ş': 's', 'Ş': 's',
	'ğ': 'g', 'Ğ': 'g',
	'ü': 'u', 'Ü': 'u', 'û': 'u', 'Û': 'u',
	'ö': 'o', 'Ö': 'o',
	'ç': 'c', 'Ç': 'c',
	'â': 'a', 'Â': 'a',
	'̇': -1, // the dot above that lower-casing İ leaves
}

// foldSearch is s as a people search compares it: lower case, with the
// Turkish letters folded (searchFold), so "Işık" matches "isik", "IŞIK" and
// "ışık", and "isik" matches "Işık".
func foldSearch(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if folded, ok := searchFold[r]; ok {
			if folded >= 0 {
				b.WriteRune(folded)
			}
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
