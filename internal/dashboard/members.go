package dashboard

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

const (
	// MembersRoot is the Group whose tree holds the club's Members
	// (CONTEXT.md, Member).
	MembersRoot = "/UYELER"
	// DefaultMembersTTL is how long one read of the Members tree is used:
	// reading it costs two Keycloak requests per Group in the tree.
	DefaultMembersTTL = 5 * time.Minute
	// MemberMonths is how many months Members.NewByMonth covers, this one
	// included.
	MemberMonths = 12
	// RecentJoiners is how many people Members.RecentJoiners lists.
	RecentJoiners = 8
)

// Members are the club's active Members: people in the UYELER tree whose
// Keycloak account is enabled and whose core account is not being erased
// or erased.
type Members struct {
	Active int `json:"active"`
	// NewByMonth counts the active Members by the month their Keycloak
	// account was created, MemberMonths months up to this one, oldest
	// first. Keycloak keeps no date for joining a Group, so this is when
	// the person registered, not when they were put in the tree.
	NewByMonth []MonthCount `json:"newByMonth"`
	// RecentJoiners are the active Members who registered last, newest
	// first.
	RecentJoiners []Joiner `json:"recentJoiners"`
	// AsOf is when the Members tree was read; it may be up to
	// DefaultMembersTTL old.
	AsOf time.Time `json:"asOf"`
}

// Joiner is the least the panel needs to show a new Member: no e-mail,
// phone or other contact.
type Joiner struct {
	ID        uuid.UUID `json:"id"`
	FirstName string    `json:"firstName"`
	LastName  string    `json:"lastName"`
	// Teams are the names of the teams under UYELER the person sits in
	// (leadership subgroups count as their team), sorted.
	Teams []string `json:"teams"`
	// RegisteredAt is when their Keycloak account was created.
	RegisteredAt time.Time `json:"registeredAt"`
}

// GroupDirectory reads the Group tree: identity.Directory.
type GroupDirectory interface {
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
	Subgroups(ctx context.Context, groupID string) ([]identity.Group, error)
	Members(ctx context.Context, groupID string) ([]identity.Person, error)
}

// treeMember is what the cache keeps of a person in the Members tree:
// nothing it does not answer, and no contact.
type treeMember struct {
	id         uuid.UUID
	firstName  string
	lastName   string
	enabled    bool
	registered *time.Time
	teams      []string
}

type memberSnapshot struct {
	at     time.Time
	people []treeMember
}

// memberCache holds one read of the Members tree for everyone allowed to
// see it: what it holds does not depend on the caller, and who may see it
// is decided on every request before it is used. Erasure is applied on
// every request too (BlockedAccounts), so an erased person never waits for
// the cache to expire.
type memberCache struct {
	dir GroupDirectory
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	snapshot *memberSnapshot
}

func newMemberCache(dir GroupDirectory, ttl time.Duration, now func() time.Time) *memberCache {
	return &memberCache{dir: dir, ttl: ttl, now: now}
}

// get answers the snapshot, reading the tree again once it is ttl old.
// Concurrent callers wait for one read; a failed read is not kept.
func (c *memberCache) get(ctx context.Context) (memberSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.snapshot != nil && now.Sub(c.snapshot.at) < c.ttl {
		return *c.snapshot, nil
	}
	people, err := readTree(ctx, c.dir)
	if err != nil {
		return memberSnapshot{}, err
	}
	c.snapshot = &memberSnapshot{at: now, people: people}
	return *c.snapshot, nil
}

// readTree reads every person in the Members tree once, with the teams of
// every Group of the tree they sit in.
func readTree(ctx context.Context, dir GroupDirectory) ([]treeMember, error) {
	root, err := dir.GetGroup(ctx, MembersRoot)
	if err != nil {
		return nil, err
	}
	groups := []identity.Group{root}
	for i := 0; i < len(groups); i++ {
		children, err := dir.Subgroups(ctx, groups[i].ID)
		if err != nil {
			return nil, err
		}
		groups = append(groups, children...)
	}
	byID := make(map[uuid.UUID]*treeMember)
	order := make([]uuid.UUID, 0)
	for _, g := range groups {
		people, err := dir.Members(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		team := teamOf(root.Path, g.Path)
		for _, p := range people {
			m, ok := byID[p.ID]
			if !ok {
				m = &treeMember{
					id: p.ID, firstName: p.FirstName, lastName: p.LastName,
					enabled: p.Enabled, registered: p.CreatedAt,
				}
				byID[p.ID] = m
				order = append(order, p.ID)
			}
			if team != "" && !slices.Contains(m.teams, team) {
				m.teams = append(m.teams, team)
			}
		}
	}
	out := make([]treeMember, 0, len(order))
	for _, id := range order {
		m := byID[id]
		sort.Strings(m.teams)
		out = append(out, *m)
	}
	return out, nil
}

// teamOf is the team a Group of the Members tree stands for: its last name
// once leadership subgroups are set aside, or "" for the root itself.
func teamOf(rootPath, path string) string {
	rest, under := strings.CutPrefix(path, rootPath+"/")
	if !under || rest == "" {
		return ""
	}
	parts := strings.Split(rest, "/")
	leaders := authz.DefaultPolicy().LeaderSubgroups
	for len(parts) > 0 && slices.Contains(leaders, parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func (s *service) memberSummary(ctx context.Context, now time.Time) (Members, error) {
	snapshot, err := s.members.get(ctx)
	if err != nil {
		return Members{}, err
	}
	ids := make([]uuid.UUID, len(snapshot.people))
	for i, m := range snapshot.people {
		ids[i] = m.id
	}
	blocked, err := s.store.BlockedAccounts(ctx, ids)
	if err != nil {
		return Members{}, err
	}
	out := Members{
		NewByMonth:    make([]MonthCount, MemberMonths),
		RecentJoiners: make([]Joiner, 0, RecentJoiners),
		AsOf:          snapshot.at.UTC(),
	}
	local := now.In(s.loc)
	this := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, s.loc)
	index := make(map[string]int, MemberMonths)
	for i := range MemberMonths {
		month := this.AddDate(0, i-(MemberMonths-1), 0).Format("2006-01")
		out.NewByMonth[i].Month = month
		index[month] = i
	}
	registered := make([]treeMember, 0)
	for _, m := range snapshot.people {
		if !m.enabled || blocked[m.id] {
			continue
		}
		out.Active++
		if m.registered == nil {
			continue
		}
		registered = append(registered, m)
		if i, ok := index[m.registered.In(s.loc).Format("2006-01")]; ok {
			out.NewByMonth[i].Count++
		}
	}
	sort.SliceStable(registered, func(i, j int) bool {
		return registered[i].registered.After(*registered[j].registered)
	})
	for _, m := range registered[:min(RecentJoiners, len(registered))] {
		teams := append([]string{}, m.teams...)
		out.RecentJoiners = append(out.RecentJoiners, Joiner{
			ID: m.id, FirstName: m.firstName, LastName: m.lastName,
			Teams: teams, RegisteredAt: m.registered.UTC(),
		})
	}
	return out, nil
}
