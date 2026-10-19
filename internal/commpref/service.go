package commpref

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"golang.org/x/sync/singleflight"
)

// Directory is the Keycloak reads the preferences need: the whole Group
// tree, a person's Groups, and whether a person still exists.
type Directory interface {
	ListGroups(ctx context.Context) ([]identity.Group, error)
	GroupsForUser(ctx context.Context, userID uuid.UUID) ([]identity.Group, error)
	GetUser(ctx context.Context, id uuid.UUID) (identity.Person, error)
}

// TreeTTL is how long the team tree read from Keycloak is used before it is
// read again. Reading it is one admin call per Group, so the screen and the
// public unsubscribe page share one copy (O1); a renamed, moved or deleted
// team shows within a minute.
const TreeTTL = time.Minute

// HiddenTeamName is how the screen names a hidden team the person is not
// in (D1).
const HiddenTeamName = "Gizli ekip"

// MaxChanges is the most changes one PUT writes.
const MaxChanges = 200

// Service keeps the preferences.
type Service struct {
	config Config
	store  Store
	dir    Directory
	roots  []string
	now    func() time.Time

	// trackingMode is TrackingMode's; onWithdrawn hears of a write that
	// queued a tracking withdrawal.
	trackingMode TrackingMode
	onWithdrawn  func()

	treeMu   sync.Mutex
	cached   *Tree
	cachedAt time.Time
	reads    singleflight.Group
}

// NewService serves the preferences of store, reading the team tree from
// dir; privilegedRoots hold no team.
func NewService(config Config, store Store, dir Directory, privilegedRoots []string) *Service {
	return &Service{config: config, store: store, dir: dir, roots: privilegedRoots, now: time.Now}
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Enabled reports whether KeyEnv is set.
func (s *Service) Enabled() bool { return s != nil && s.config.Enabled }

// Config is the service's settings.
func (s *Service) Config() Config { return s.config }

// UnsubscribeURL is Config.UnsubscribeURL.
func (s *Service) UnsubscribeURL(person uuid.UUID, ch Channel, cats []Category) (string, error) {
	return s.config.UnsubscribeURL(person, ch, cats)
}

// tree is the team tree, read from Keycloak at most once a TreeTTL;
// concurrent misses share one read.
func (s *Service) tree(ctx context.Context) (*Tree, error) {
	now := s.now()
	s.treeMu.Lock()
	if s.cached != nil && now.Sub(s.cachedAt) < TreeTTL {
		tree := s.cached
		s.treeMu.Unlock()
		return tree, nil
	}
	s.treeMu.Unlock()
	read, err, _ := s.reads.Do("tree", func() (any, error) {
		groups, err := s.dir.ListGroups(ctx)
		if err != nil {
			return nil, err
		}
		tree := NewTree(groups, s.roots)
		s.treeMu.Lock()
		s.cached, s.cachedAt = tree, now
		s.treeMu.Unlock()
		return tree, nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTeamsUnavailable, err)
	}
	return read.(*Tree), nil
}

// Cell is one switch on the screen.
type Cell struct {
	// Explicit is the person's own choice on this switch, null when none.
	Explicit *bool `json:"explicit"`
	// Enabled is the switch's value, the master switch not applied.
	Enabled bool   `json:"enabled"`
	Origin  Origin `json:"origin"`
	// From is the team the value comes from when it is not the switch's
	// own: an explicit choice above it (Karar D5) or, for a default, the
	// team above the person is in (Y1).
	From string `json:"from,omitempty"`
	// Effective is whether the message goes: enabled and the channel's
	// master switch on.
	Effective bool `json:"effective"`
}

// ChannelCells is one category's switch on every channel ({"email": …}).
type ChannelCells map[Channel]Cell

// KindCells is a source's two categories (Karar D6).
type KindCells struct {
	Duyuru ChannelCells `json:"duyuru"`
	Haber  ChannelCells `json:"haber"`
}

// TeamRow is a team on the screen, with its sub-teams.
type TeamRow struct {
	GroupID     string `json:"groupId"`
	Code        string `json:"code"`
	DisplayName string `json:"displayName"`
	Path        string `json:"path"`
	// Member is whether the person is in the team (or a team below it).
	Member bool `json:"member"`
	// Hidden marks a hidden team the person is not in but has a choice on:
	// its code and path are left out and its name is HiddenTeamName. Its
	// switches are off; forgetting the choice (null) removes the row.
	Hidden   bool      `json:"hidden,omitempty"`
	Kinds    KindCells `json:"kinds"`
	Children []TeamRow `json:"children"`
}

// TeamRef names a team that may be followed.
type TeamRef struct {
	GroupID     string `json:"groupId"`
	Code        string `json:"code"`
	DisplayName string `json:"displayName"`
	Path        string `json:"path"`
}

// Screen is the Account center's "İletişim tercihleri" model. Every
// switch is given per channel, so the screen can show categories times
// channels; today e-mail is the only channel.
type Screen struct {
	// Channels are the channels of every switch below, in order.
	Channels []Channel `json:"channels"`
	// Master is each channel's master switch ("all").
	Master ChannelCells `json:"master"`
	// Club is club-wide mail.
	Club KindCells `json:"club"`
	// Teams are the person's teams, as trees from their top teams, every
	// sub-team included.
	Teams []TeamRow `json:"teams"`
	// Following are the other teams the person made a choice on (Karar
	// D7: following is off by default), as trees.
	Following []TeamRow `json:"following"`
	// Followable are the publicly listed teams outside both lists.
	Followable []TeamRef `json:"followable"`
	// Tracking is the open and click tracking switch (hub ticket 16): no
	// channel, no category of the lists above.
	Tracking TrackingState `json:"tracking"`
}

func cell(e Explicit, v Value, c Category) Cell {
	out := Cell{Enabled: v.Enabled, Origin: v.Origin, From: v.From, Effective: v.Effective}
	if on, ok := e[c]; ok {
		out.Explicit = &on
	}
	return out
}

// Mine is the screen of person.
func (s *Service) Mine(ctx context.Context, person uuid.UUID) (Screen, error) {
	if !s.Enabled() {
		return Screen{}, ErrDisabled
	}
	tree, err := s.tree(ctx)
	if err != nil {
		return Screen{}, err
	}
	groups, err := s.dir.GroupsForUser(ctx, person)
	if err != nil {
		return Screen{}, fmt.Errorf("%w: %v", ErrTeamsUnavailable, err)
	}
	settings, err := s.store.List(ctx, person)
	if err != nil {
		return Screen{}, err
	}
	return s.screen(tree, tree.Membership(identity.GroupPaths(groups)), settings), nil
}

// screen is the screen with its tracking switch.
func (s *Service) screen(tree *Tree, m Membership, settings []Setting) Screen {
	out := screen(tree, m, settings)
	out.Tracking = trackingState(settings, s.TrackingMode(), m.Member)
	return out
}

func screen(tree *Tree, m Membership, settings []Setting) Screen {
	out := Screen{Channels: slices.Clone(Channels), Teams: []TeamRow{}, Following: []TeamRow{}, Followable: []TeamRef{}}
	explicit := map[Channel]Explicit{}
	for _, ch := range Channels {
		explicit[ch] = ExplicitOf(settings, ch)
	}
	cells := func(c Category) ChannelCells {
		out := ChannelCells{}
		for _, ch := range Channels {
			out[ch] = cell(explicit[ch], tree.Resolve(explicit[ch], m, c), c)
		}
		return out
	}
	kinds := func(source Category) KindCells {
		duyuru, haber := source, source
		duyuru.Kind, haber.Kind = KindAnnouncement, KindNews
		return KindCells{Duyuru: cells(duyuru), Haber: cells(haber)}
	}
	out.Master = cells(Master())
	out.Club = kinds(Category{Scope: ScopeClub})
	var row func(id string) TeamRow
	row = func(id string) TeamRow {
		node := tree.teams[id]
		r := TeamRow{
			GroupID: id, Code: node.Code, DisplayName: node.DisplayName, Path: node.Path, Member: m.Teams[id],
			Kinds:    kinds(Category{Scope: ScopeTeam, GroupID: id}),
			Children: []TeamRow{},
		}
		for _, child := range node.Children {
			// A hidden sub-team the person is not in stays out (O2).
			if tree.seen(m, child) {
				r.Children = append(r.Children, row(child))
			}
		}
		return r
	}
	shown := map[string]bool{}
	// Own trees: the top teams the person is in.
	for _, root := range tree.roots {
		if m.Teams[root] {
			r := row(root)
			out.Teams = append(out.Teams, r)
			markShown(r, shown)
		}
	}
	// Following: the teams not shown yet with a choice on any channel, as
	// trees from the topmost one. A hidden team the person is not in (a
	// choice left from an old link, or written by id) is shown masked, so
	// the choice can still be forgotten, and names nothing (D1).
	var chosen []string
	for _, setting := range settings {
		c := setting.Category
		if _, ok := tree.teams[c.GroupID]; ok && c.Scope == ScopeTeam && !shown[c.GroupID] && !slices.Contains(chosen, c.GroupID) {
			chosen = append(chosen, c.GroupID)
		}
	}
	slices.SortFunc(chosen, func(a, b string) int { return strings.Compare(tree.teams[a].Path, tree.teams[b].Path) })
	for _, id := range chosen {
		if shown[id] {
			continue
		}
		if !tree.seen(m, id) {
			r := TeamRow{
				GroupID: id, DisplayName: HiddenTeamName, Hidden: true,
				Kinds:    kinds(Category{Scope: ScopeTeam, GroupID: id}),
				Children: []TeamRow{},
			}
			out.Following = append(out.Following, r)
			shown[id] = true
			continue
		}
		r := row(id)
		out.Following = append(out.Following, r)
		markShown(r, shown)
	}
	// Followable: public teams in neither list; none for someone who is no
	// Member (only a Member follows, Karar D7).
	var public []string
	for id, node := range tree.teams {
		if m.Member && node.Public && !shown[id] {
			public = append(public, id)
		}
	}
	slices.SortFunc(public, func(a, b string) int { return strings.Compare(tree.teams[a].Path, tree.teams[b].Path) })
	for _, id := range public {
		node := tree.teams[id]
		out.Followable = append(out.Followable, TeamRef{GroupID: id, Code: node.Code, DisplayName: node.DisplayName, Path: node.Path})
	}
	return out
}

func markShown(r TeamRow, shown map[string]bool) {
	shown[r.GroupID] = true
	for _, child := range r.Children {
		markShown(child, shown)
	}
}

// Update writes a person's changes from the Account center and answers the
// new screen. A change of Tracking() (e-mail channel, true or false, never
// forgotten) is the tracking switch; turning it off queues the withdrawal
// SkyMail must hear of, in the same transaction. A team the person turns on or off must be one they may see:
// one they are in, or a listed one (Karar D7, O2); any other, hidden or
// not there, is ErrUnknownTeam alike. Forgetting a choice is always
// allowed, so a hidden or deleted team's row can be cleared.
func (s *Service) Update(ctx context.Context, person uuid.UUID, changes []Change) (Screen, error) {
	if !s.Enabled() {
		return Screen{}, ErrDisabled
	}
	if len(changes) == 0 || len(changes) > MaxChanges {
		return Screen{}, ErrInvalid
	}
	type key struct {
		ch Channel
		c  Category
	}
	seen := map[key]bool{}
	withdrawn := false
	for _, change := range changes {
		k := key{change.Channel, change.Category}
		if change.Category == Tracking() {
			if change.Channel != ChannelEmail || change.Enabled == nil || seen[k] {
				return Screen{}, ErrInvalid
			}
			seen[k] = true
			withdrawn = withdrawn || !*change.Enabled
			continue
		}
		if !change.Channel.valid() || !change.Category.Valid() || seen[k] {
			return Screen{}, ErrInvalid
		}
		seen[k] = true
	}
	tree, err := s.tree(ctx)
	if err != nil {
		return Screen{}, err
	}
	groups, err := s.dir.GroupsForUser(ctx, person)
	if err != nil {
		return Screen{}, fmt.Errorf("%w: %v", ErrTeamsUnavailable, err)
	}
	m := tree.Membership(identity.GroupPaths(groups))
	for _, change := range changes {
		c := change.Category
		if c.Scope != ScopeTeam || change.Enabled == nil {
			continue
		}
		// A hidden team the person is not in answers as one that does not
		// exist, on or off, so the answer tells nothing of it (D1).
		if _, ok := tree.teams[c.GroupID]; !ok || !tree.seen(m, c.GroupID) {
			return Screen{}, ErrUnknownTeam
		}
		// Following is a Member's (Karar D7): someone in no /UYELER
		// Group cannot turn a team on. A follower who is no Member would
		// be Faz 2's subscriber, on a Contact consent (SPEC §5.8, K8).
		// Turning off and forgetting stay allowed.
		if *change.Enabled && !m.Member {
			return Screen{}, ErrFollowMembersOnly
		}
	}
	if err := s.store.Apply(ctx, person, changes, SourceAccountCenter, s.now().UTC()); err != nil {
		return Screen{}, err
	}
	if withdrawn && s.onWithdrawn != nil {
		s.onWithdrawn()
	}
	settings, err := s.store.List(ctx, person)
	if err != nil {
		return Screen{}, err
	}
	return s.screen(tree, m, settings), nil
}

// LinkCategory is one category a link names, with how the page shows it.
type LinkCategory struct {
	Category Category
	// Source is the source's name: "Kulüp geneli", or the team's code.
	Source string
	// Label is "AIRLAB · Duyurular".
	Label string
}

// Link is what an unsubscribe link names.
type Link struct {
	Person     uuid.UUID
	Channel    Channel
	Categories []LinkCategory
}

// kindLabel is how a page names a kind's mail.
func kindLabel(k Kind) string {
	if k == KindAnnouncement {
		return "Duyurular"
	}
	return "Haberler"
}

// ReadLink reads the link token and names its categories. The team names
// come from Keycloak; when it does not answer, or a team is gone, the page
// still works with a general name: the link turns off what it carries
// whatever the names.
func (s *Service) ReadLink(ctx context.Context, token string) (Link, error) {
	person, ch, cats, err := s.config.readToken(token)
	if err != nil {
		return Link{}, err
	}
	var tree *Tree
	if s.dir != nil {
		if read, err := s.tree(ctx); err == nil {
			tree = read
		}
	}
	link := Link{Person: person, Channel: ch}
	for _, c := range cats {
		source := "Kulüp geneli"
		if c.Scope == ScopeTeam {
			source = "Bir ekip"
			if tree != nil {
				if node, ok := tree.teams[c.GroupID]; ok {
					source = node.Code
				}
			}
		}
		link.Categories = append(link.Categories, LinkCategory{Category: c, Source: source, Label: source + " · " + kindLabel(c.Kind)})
	}
	return link, nil
}

// Reach is how much an unsubscribe turns off.
type Reach string

const (
	// ReachCategories turns off exactly the link's categories: what a
	// one-click unsubscribe (RFC 8058) does.
	ReachCategories Reach = "categories"
	// ReachSources turns off both kinds of every source the link names
	// ("bu kaynaktan her şeyi kapat").
	ReachSources Reach = "sources"
	// ReachAll turns the link channel's master switch off ("tümünden
	// çık").
	ReachAll Reach = "all"
)

// ParseReach reads a page's choice; empty is ReachCategories.
func ParseReach(raw string) (Reach, error) {
	switch Reach(raw) {
	case "", ReachCategories:
		return ReachCategories, nil
	case ReachSources, ReachAll:
		return Reach(raw), nil
	}
	return "", ErrInvalid
}

// Unsubscribe turns off what reach says for the person the link names: it
// writes enabled=false choices (source unsubscribe_link) and nothing else.
// It can be repeated: a second call leaves the same choices. A person being
// erased or erased is ErrSubjectInactive: nothing is written, and nothing
// will be mailed to them either.
func (s *Service) Unsubscribe(ctx context.Context, token string, reach Reach) (Link, error) {
	person, ch, cats, err := s.config.readToken(token)
	if err != nil {
		return Link{}, err
	}
	off := false
	var changes []Change
	switch reach {
	case ReachAll:
		changes = []Change{{Channel: ch, Category: Master(), Enabled: &off}}
	case ReachSources:
		seen := map[Category]bool{}
		for _, c := range cats {
			for _, k := range Kinds {
				wide := c.source()
				wide.Kind = k
				if !seen[wide] {
					seen[wide] = true
					changes = append(changes, Change{Channel: ch, Category: wide, Enabled: &off})
				}
			}
		}
	case ReachCategories:
		for _, c := range cats {
			changes = append(changes, Change{Channel: ch, Category: c, Enabled: &off})
		}
	default:
		return Link{}, ErrInvalid
	}
	if err := s.store.Apply(ctx, person, changes, SourceUnsubscribeLink, s.now().UTC()); err != nil {
		return Link{}, err
	}
	link := Link{Person: person, Channel: ch}
	for _, c := range cats {
		link.Categories = append(link.Categories, LinkCategory{Category: c})
	}
	return link, nil
}

// EraseSubject deletes every preference of person: account erasure's
// erase_communication_preferences step. It works whether preferences are on or off,
// and a second call finds nothing.
func (s *Service) EraseSubject(ctx context.Context, person uuid.UUID) (int64, error) {
	return s.store.EraseSubject(ctx, person)
}

// errIncompleteTree is a Group list without the members' root: Keycloak
// answered, but not the tree core knows, so nothing is deleted on it.
var errIncompleteTree = errors.New("communication preferences: the Group list has no " + authz.MembersRoot + "; no deleted team removed")

// RemoveDeletedTeams deletes the rows of teams whose Group is gone from
// Keycloak. A Group list without /UYELER deletes nothing.
func (s *Service) RemoveDeletedTeams(ctx context.Context) (int64, error) {
	ids, err := s.store.TeamGroupIDs(ctx)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	groups, err := s.dir.ListGroups(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrTeamsUnavailable, err)
	}
	exists := map[string]bool{}
	rooted := false
	for _, g := range groups {
		exists[g.ID] = true
		rooted = rooted || g.Path == authz.MembersRoot
	}
	if !rooted {
		return 0, errIncompleteTree
	}
	var gone []string
	for _, id := range ids {
		if !exists[id] {
			gone = append(gone, id)
		}
	}
	return s.store.DeleteTeams(ctx, gone)
}

// errTooManyGone is a run that found more people gone than a deletion
// outside the saga explains: Keycloak answering for the wrong realm, say.
var errTooManyGone = errors.New("communication preferences: too many people unknown to Keycloak; none removed")

// RemoveDeletedPeople deletes the rows of people Keycloak no longer has: an
// account deleted outside the erasure saga (straight from the Keycloak
// console) keeps no preference (D2). The saga deletes its own rows in its
// step. A Keycloak error other than "not found" stops the run with nothing
// deleted, and so does a run that finds more than a quarter of the people
// gone (and more than five).
func (s *Service) RemoveDeletedPeople(ctx context.Context) (int64, error) {
	people, err := s.store.Subjects(ctx)
	if err != nil || len(people) == 0 {
		return 0, err
	}
	var gone []uuid.UUID
	for _, person := range people {
		_, err := s.dir.GetUser(ctx, person)
		switch {
		case errors.Is(err, identity.ErrNotFound):
			gone = append(gone, person)
		case err != nil:
			return 0, fmt.Errorf("communication preferences: reading a person from Keycloak: %w", err)
		}
	}
	if len(gone) > 5 && len(gone)*4 > len(people) {
		return 0, errTooManyGone
	}
	var removed int64
	for _, person := range gone {
		n, err := s.store.EraseSubject(ctx, person)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// CleanupInterval is how often the rows of deleted teams and people are
// removed.
const CleanupInterval = 24 * time.Hour

// MaintainCleanup removes the rows of deleted teams and of deleted people
// now and then every interval until ctx ends. report hears of each part
// ("deleted teams", "deleted people") that removed rows or failed. The
// returned channel closes when it has stopped.
func MaintainCleanup(ctx context.Context, s *Service, interval time.Duration, report func(what string, removed int64, err error)) <-chan struct{} {
	done := make(chan struct{})
	run := func() {
		for _, part := range []struct {
			what string
			run  func(context.Context) (int64, error)
		}{{"deleted teams", s.RemoveDeletedTeams}, {"deleted people", s.RemoveDeletedPeople}} {
			n, err := part.run(ctx)
			if report != nil && (err != nil || n > 0) {
				report(part.what, n, err)
			}
		}
	}
	go func() {
		defer close(done)
		run()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	return done
}
