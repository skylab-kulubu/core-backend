package dashboard

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// MembersRoot is the Group whose tree holds the club's Members
	// (CONTEXT.md, Member).
	MembersRoot = "/UYELER"
	// DefaultMembersTTL is how long one read of the Members tree is used
	// before a new one starts: reading it costs about two Keycloak requests
	// per Group in the tree.
	DefaultMembersTTL = 5 * time.Minute
	// DefaultMembersReadTimeout bounds one read of the tree as a whole.
	DefaultMembersReadTimeout = 5 * time.Second
	// DefaultMembersFailureTTL is how long a failed read is kept: no new read
	// starts before it passes.
	DefaultMembersFailureTTL = 30 * time.Second
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
	// AsOf is when the Members tree was read: usually less than
	// DefaultMembersTTL ago, older while Keycloak cannot be read.
	AsOf time.Time `json:"asOf"`
}

// Joiner is the least the panel needs to show a new Member: no e-mail,
// phone or other contact.
type Joiner struct {
	ID        uuid.UUID `json:"id"`
	FirstName string    `json:"firstName"`
	LastName  string    `json:"lastName"`
	// Teams are the names of the teams under UYELER the person sits in
	// (leadership subgroups count as their team), sorted. A caller who may
	// not manage people (User Update) gets the publicly listed ones only.
	Teams []string `json:"teams"`
	// RegisteredAt is when their Keycloak account was created.
	RegisteredAt time.Time `json:"registeredAt"`
	// ProfilePictureURL and ProfilePictureSizes are the person's profile
	// picture as the public team list and /v1/users/me answer it: its
	// address and its card and page addresses. Omitted for a person without
	// a picture.
	ProfilePictureURL   string                        `json:"profilePictureUrl,omitempty"`
	ProfilePictureSizes map[string]media.ImageAddress `json:"profilePictureSizes,omitempty"`
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
	teams      []treeTeam
}

// treeTeam is a team a person sits in and whether it is publicly listed
// (the public_listing attribute of its Group).
type treeTeam struct {
	name   string
	public bool
}

type memberSnapshot struct {
	at     time.Time
	people []treeMember
}

// errMembersUnavailable answers a caller while no read of the tree has
// succeeded: the last read failed and its failure is still kept, or the
// caller stopped waiting.
var errMembersUnavailable = errors.New("dashboard: the Members tree could not be read")

// memberCache holds one read of the Members tree for everyone allowed to
// see it: what it holds does not depend on the caller, and who may see it
// is decided on every request before it is used. Erasure is applied on
// every request too (Store.Accounts), so an erased person never waits for
// the cache.
//
// One read runs at a time, detached from the requests that wait for it and
// bounded by readTimeout; a request that goes away stops waiting without
// failing the others. Once a read is ttl old the next request starts a new
// one in the background and is answered the old one meanwhile. A failed
// read leaves the last good one in place (served on, however old) and is
// kept for failureTTL: no new read starts before that.
type memberCache struct {
	dir         GroupDirectory
	ttl         time.Duration
	readTimeout time.Duration
	failureTTL  time.Duration
	now         func() time.Time
	logf        func(format string, args ...any)

	mu       sync.Mutex
	snapshot *memberSnapshot
	failedAt *time.Time
	reading  chan struct{}
}

// get answers the last good read, starting a new one when it is due, and
// waits for that read only when there is no good read yet.
func (c *memberCache) get(ctx context.Context) (memberSnapshot, error) {
	c.mu.Lock()
	now := c.now()
	failing := c.failedAt != nil && now.Sub(*c.failedAt) < c.failureTTL
	if c.snapshot != nil {
		if now.Sub(c.snapshot.at) >= c.ttl && !failing {
			c.startLocked()
		}
		snapshot := *c.snapshot
		c.mu.Unlock()
		return snapshot, nil
	}
	if failing {
		c.mu.Unlock()
		return memberSnapshot{}, errMembersUnavailable
	}
	reading := c.startLocked()
	c.mu.Unlock()

	select {
	case <-reading:
	case <-ctx.Done():
		return memberSnapshot{}, errMembersUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshot == nil {
		return memberSnapshot{}, errMembersUnavailable
	}
	return *c.snapshot, nil
}

// startLocked starts a read unless one is running, and answers the channel
// that closes when the running read ends. The caller holds c.mu.
func (c *memberCache) startLocked() chan struct{} {
	if c.reading != nil {
		return c.reading
	}
	done := make(chan struct{})
	c.reading = done
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), c.readTimeout)
		people, err := readTree(ctx, c.dir)
		cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.reading = nil
		at := c.now()
		if err != nil {
			c.failedAt = &at
			// The error is the directory's: Keycloak's status or a
			// timeout. It names no person; no id, name or path is added.
			c.logf(`{"event":"dashboard_members","outcome":"unavailable","stale":%t,"error":%q}`, c.snapshot != nil, err.Error())
			return
		}
		c.failedAt = nil
		c.snapshot = &memberSnapshot{at: at, people: people}
	}()
	return done
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
	byPath := make(map[string]identity.Group, len(groups))
	for _, g := range groups {
		byPath[g.Path] = g
	}
	byID := make(map[uuid.UUID]*treeMember)
	order := make([]uuid.UUID, 0)
	for _, g := range groups {
		people, err := dir.Members(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		teamPath := teamOf(root.Path, g.Path)
		var team treeTeam
		if teamPath != "" {
			team = treeTeam{
				name:   teamPath[strings.LastIndex(teamPath, "/")+1:],
				public: strings.EqualFold(byPath[teamPath].Attributes["public_listing"], "true"),
			}
		}
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
			if team.name != "" && !slices.Contains(m.teams, team) {
				m.teams = append(m.teams, team)
			}
		}
	}
	out := make([]treeMember, 0, len(order))
	for _, id := range order {
		m := byID[id]
		sort.Slice(m.teams, func(i, j int) bool { return m.teams[i].name < m.teams[j].name })
		out = append(out, *m)
	}
	return out, nil
}

// teamOf is the path of the team a Group of the Members tree stands for:
// the Group itself once leadership subgroups are set aside, or "" for the
// root.
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
	return rootPath + "/" + strings.Join(parts, "/")
}

// memberSummary answers the section from the cached tree and core's
// accounts. allTeams is false for a caller who may read people but not
// manage them: they see the publicly listed teams only.
func (s *service) memberSummary(ctx context.Context, now time.Time, allTeams bool) (Members, error) {
	snapshot, err := s.members.get(ctx)
	if err != nil {
		return Members{}, err
	}
	ids := make([]uuid.UUID, len(snapshot.people))
	for i, m := range snapshot.people {
		ids[i] = m.id
	}
	accounts, err := s.store.Accounts(ctx, ids)
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
		if !m.enabled || accounts[m.id].Blocked {
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
	// The summary has no media dependency to carry a base and mode of its
	// own: the ones core is configured with, as the team list's.
	addresses := media.ConfiguredAddresses()
	for _, m := range registered[:min(RecentJoiners, len(registered))] {
		joiner := Joiner{
			ID: m.id, FirstName: m.firstName, LastName: m.lastName,
			Teams: make([]string, 0, len(m.teams)), RegisteredAt: m.registered.UTC(),
		}
		// As core's other people reads: the names core stores win.
		if account := accounts[m.id]; account.Stored {
			joiner.FirstName, joiner.LastName = account.FirstName, account.LastName
			// A person core may no longer show (Blocked) is no joiner at
			// all; the placeholder subject is nobody's picture.
			if m.id != user.DeletedSubject {
				joiner.ProfilePictureURL = addresses.Object(account.ProfilePictureKey)
				joiner.ProfilePictureSizes = addresses.LinkedSizes(account.ProfilePicture)
			}
		}
		for _, team := range m.teams {
			if allTeams || team.public {
				joiner.Teams = append(joiner.Teams, team.name)
			}
		}
		out.RecentJoiners = append(out.RecentJoiners, joiner)
	}
	return out, nil
}
