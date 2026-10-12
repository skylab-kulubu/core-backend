package mailpref

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// Directory is the Keycloak reads the preferences need: the whole Group
// tree, and a person's Groups.
type Directory interface {
	ListGroups(ctx context.Context) ([]identity.Group, error)
	GroupsForUser(ctx context.Context, userID uuid.UUID) ([]identity.Group, error)
}

// MaxChanges is the most changes one PUT writes.
const MaxChanges = 200

// Service keeps the preferences.
type Service struct {
	config Config
	store  Store
	dir    Directory
	roots  []string
	now    func() time.Time
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
func (s *Service) UnsubscribeURL(person uuid.UUID, cats []Category) (string, error) {
	return s.config.UnsubscribeURL(person, cats)
}

func (s *Service) tree(ctx context.Context) (*Tree, []identity.Group, error) {
	groups, err := s.dir.ListGroups(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrTeamsUnavailable, err)
	}
	return NewTree(groups, s.roots), groups, nil
}

// Cell is one switch on the screen.
type Cell struct {
	// Explicit is the person's own choice on this switch, null when none.
	Explicit *bool `json:"explicit"`
	// Enabled is the switch's value, the master switch not applied.
	Enabled bool   `json:"enabled"`
	Origin  Origin `json:"origin"`
	// From is the team an inherited value comes from (Karar D5).
	From string `json:"from,omitempty"`
	// Effective is whether the mail goes: enabled and the master switch on.
	Effective bool `json:"effective"`
}

// KindCells is a source's two switches (Karar D6).
type KindCells struct {
	Duyuru Cell `json:"duyuru"`
	Haber  Cell `json:"haber"`
}

// TeamRow is a team on the screen, with its sub-teams.
type TeamRow struct {
	GroupID     string `json:"groupId"`
	Code        string `json:"code"`
	DisplayName string `json:"displayName"`
	Path        string `json:"path"`
	// Member is whether the person is in the team (or a team below it).
	Member   bool      `json:"member"`
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

// Screen is the Account center's "İletişim tercihleri" model.
type Screen struct {
	// Master is the master switch ("all").
	Master Cell `json:"master"`
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
	tree, _, err := s.tree(ctx)
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
	return screen(tree, tree.Membership(identity.GroupPaths(groups)), ExplicitOf(settings)), nil
}

func screen(tree *Tree, m Membership, e Explicit) Screen {
	out := Screen{Teams: []TeamRow{}, Following: []TeamRow{}, Followable: []TeamRef{}}
	out.Master = cell(e, tree.Resolve(e, m, Master()), Master())
	out.Club = KindCells{
		Duyuru: cell(e, tree.Resolve(e, m, Club(KindAnnouncement)), Club(KindAnnouncement)),
		Haber:  cell(e, tree.Resolve(e, m, Club(KindNews)), Club(KindNews)),
	}
	var row func(id string) TeamRow
	row = func(id string) TeamRow {
		node := tree.teams[id]
		duyuru, haber := Team(id, KindAnnouncement), Team(id, KindNews)
		r := TeamRow{
			GroupID: id, Code: node.Code, DisplayName: node.DisplayName, Path: node.Path, Member: m.Teams[id],
			Kinds: KindCells{
				Duyuru: cell(e, tree.Resolve(e, m, duyuru), duyuru),
				Haber:  cell(e, tree.Resolve(e, m, haber), haber),
			},
			Children: []TeamRow{},
		}
		for _, child := range node.Children {
			// A followed team's hidden sub-team stays hidden; one of the
			// person's own teams shows every sub-team.
			if m.Teams[tree.root(id)] || tree.visible(child) {
				r.Children = append(r.Children, row(child))
			}
		}
		return r
	}
	// Own trees: the top teams the person is in.
	for _, root := range tree.roots {
		if m.Teams[root] {
			out.Teams = append(out.Teams, row(root))
		}
	}
	// Following: the topmost teams outside the own trees with a choice.
	chosen := map[string]bool{}
	for c := range e {
		if c.Scope != ScopeTeam {
			continue
		}
		if _, ok := tree.teams[c.GroupID]; ok && !m.Teams[tree.root(c.GroupID)] {
			chosen[c.GroupID] = true
		}
	}
	var followed []string
	for id := range chosen {
		covered := false
		for _, above := range tree.chain(id)[1:] {
			if chosen[above] {
				covered = true
				break
			}
		}
		if !covered {
			followed = append(followed, id)
		}
	}
	slices.SortFunc(followed, func(a, b string) int { return strings.Compare(tree.teams[a].Path, tree.teams[b].Path) })
	shown := map[string]bool{}
	for _, id := range followed {
		r := row(id)
		out.Following = append(out.Following, r)
		markShown(r, shown)
	}
	// Followable: public teams in neither list.
	var public []string
	for id, node := range tree.teams {
		if node.Public && !m.Teams[tree.root(id)] && !shown[id] {
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
// new screen. A team the person turns on must be in their own trees or
// visible (publicly listed, or below a publicly listed team: Karar D7); a
// team turned off must exist; forgetting a choice is always allowed, so a
// deleted team's row can be cleared.
func (s *Service) Update(ctx context.Context, person uuid.UUID, changes []Change) (Screen, error) {
	if !s.Enabled() {
		return Screen{}, ErrDisabled
	}
	if len(changes) == 0 || len(changes) > MaxChanges {
		return Screen{}, ErrInvalid
	}
	seen := map[Category]bool{}
	for _, change := range changes {
		if !change.Category.Valid() || seen[change.Category] {
			return Screen{}, ErrInvalid
		}
		seen[change.Category] = true
	}
	tree, _, err := s.tree(ctx)
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
		if _, ok := tree.teams[c.GroupID]; !ok {
			return Screen{}, ErrUnknownTeam
		}
		if *change.Enabled && !m.Teams[tree.root(c.GroupID)] && !tree.visible(c.GroupID) {
			return Screen{}, ErrUnknownTeam
		}
	}
	if err := s.store.Apply(ctx, person, changes, SourceAccountCenter, s.now().UTC()); err != nil {
		return Screen{}, err
	}
	settings, err := s.store.List(ctx, person)
	if err != nil {
		return Screen{}, err
	}
	return screen(tree, m, ExplicitOf(settings)), nil
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
	person, cats, err := s.config.readToken(token)
	if err != nil {
		return Link{}, err
	}
	var tree *Tree
	if s.dir != nil {
		if groups, err := s.dir.ListGroups(ctx); err == nil {
			tree = NewTree(groups, s.roots)
		}
	}
	link := Link{Person: person}
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
	// ReachAll turns the master switch off ("tümünden çık").
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
	person, cats, err := s.config.readToken(token)
	if err != nil {
		return Link{}, err
	}
	off := false
	var changes []Change
	switch reach {
	case ReachAll:
		changes = []Change{{Category: Master(), Enabled: &off}}
	case ReachSources:
		seen := map[Category]bool{}
		for _, c := range cats {
			for _, k := range Kinds {
				wide := c.source()
				wide.Kind = k
				if !seen[wide] {
					seen[wide] = true
					changes = append(changes, Change{Category: wide, Enabled: &off})
				}
			}
		}
	case ReachCategories:
		for _, c := range cats {
			changes = append(changes, Change{Category: c, Enabled: &off})
		}
	default:
		return Link{}, ErrInvalid
	}
	if err := s.store.Apply(ctx, person, changes, SourceUnsubscribeLink, s.now().UTC()); err != nil {
		return Link{}, err
	}
	link := Link{Person: person}
	for _, c := range cats {
		link.Categories = append(link.Categories, LinkCategory{Category: c})
	}
	return link, nil
}

// EraseSubject deletes every preference of person: account erasure's
// erase_mail_preferences step. It works whether preferences are on or off,
// and a second call finds nothing.
func (s *Service) EraseSubject(ctx context.Context, person uuid.UUID) (int64, error) {
	return s.store.EraseSubject(ctx, person)
}

// errIncompleteTree is a Group list without the members' root: Keycloak
// answered, but not the tree core knows, so nothing is deleted on it.
var errIncompleteTree = errors.New("mail preferences: the Group list has no " + authz.MembersRoot + "; no deleted team removed")

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

// CleanupInterval is how often the deleted teams' rows are removed.
const CleanupInterval = 24 * time.Hour

// MaintainDeletedTeams removes the deleted teams' rows now and then every
// interval until ctx ends. The returned channel closes when it has stopped.
func MaintainDeletedTeams(ctx context.Context, s *Service, interval time.Duration, report func(removed int64, err error)) <-chan struct{} {
	done := make(chan struct{})
	run := func() {
		n, err := s.RemoveDeletedTeams(ctx)
		if report != nil && (err != nil || n > 0) {
			report(n, err)
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
