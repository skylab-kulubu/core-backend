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
	"github.com/skylab-kulubu/core-backend/internal/user"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// AudienceRole is core's client role of the service account that may
// resolve a News send's audience: SkyMail's (bilet 04). A person's token
// never may, whatever roles it carries.
const AudienceRole = "audience:read"

// AudienceType is who a News send is addressed to.
type AudienceType string

const (
	// AudienceMembers is every Member: everyone in the /UYELER tree.
	AudienceMembers AudienceType = "members"
	// AudienceTeams is the named teams: their members (with their
	// sub-teams'), and the people who follow them or a team above them.
	AudienceTeams AudienceType = "teams"
)

// Visibility is who may read the News record on the web (ADR 0067 karar 2).
// A send never reaches a person the record is not visible to.
type Visibility string

const (
	VisibilityPublic  Visibility = "herkes"
	VisibilityMembers Visibility = "uyeler"
	VisibilityTeams   Visibility = "ekipler"
)

// PrivateVisibilityEnv turns on the private visibilities (uyeler, ekipler):
// the second step of ADR 0067 karar 3. Unset or "off": only herkes is
// accepted (Faz 1, every News record public); "on": all three.
const PrivateVisibilityEnv = "NEWS_AUDIENCE_PRIVATE_VISIBILITY"

// PrivateVisibilityFromEnv reads PrivateVisibilityEnv.
func PrivateVisibilityFromEnv(getenv func(string) string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(getenv(PrivateVisibilityEnv))) {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	}
	return false, fmt.Errorf("%s must be on or off", PrivateVisibilityEnv)
}

const (
	// MaxAudienceTeams is the most teams one request names, either list:
	// every category a recipient enters through must fit one link.
	MaxAudienceTeams = MaxLinkCategories
	// MaxAudiencePage is the largest page of recipients.
	MaxAudiencePage = 500
	// AudienceSnapshotTTL is how long one read of the /UYELER tree's
	// members serves: a live count typed against a form, and the pages of
	// one send, share it.
	AudienceSnapshotTTL = 30 * time.Second
	// audienceReaders bounds the Group member reads in flight.
	audienceReaders = 4
)

// AudienceStartupLines are the startup log lines of the News audience: its
// state, and a warning while the private visibilities are on, which is
// meant only after inscribed's per-record visibility (F2) and the leak
// tests (duyuru-haber bilet 21) are green.
func AudienceStartupLines(enabled, private bool) []string {
	state := "off"
	if enabled {
		state = "on"
	}
	lines := []string{fmt.Sprintf("news audience: %s (private visibility %t)", state, private)}
	if private {
		lines = append(lines, "WARNING news audience: "+PrivateVisibilityEnv+"=on accepts uyeler and ekipler records; "+
			"turn it on only after inscribed's per-record visibility (F2) is in production and the leak tests (duyuru-haber bilet 21) are green (ADR 0067 karar 3)")
	}
	return lines
}

// ErrVisibilityNotEnabled is uyeler or ekipler while PrivateVisibilityEnv
// is off.
var ErrVisibilityNotEnabled = errors.New("communication preference: private visibility is not enabled")

// AudienceRequest is one News send's audience question.
type AudienceRequest struct {
	Type AudienceType
	// Teams are the teams of an AudienceTeams send, by Group path or id.
	Teams []string
	Kind  Kind
	// Visibility is the record's; empty is herkes (the field's absence
	// means public, ADR 0067 karar 3).
	Visibility Visibility
	// VisibilityTeams are the teams an ekipler record is visible to, by
	// Group path or id; with their sub-teams.
	VisibilityTeams []string
}

// AudienceDirectory is the Keycloak reads of the audience: the Group tree,
// each Group's direct members, and one person outside /UYELER (a follower
// who is no Member).
type AudienceDirectory interface {
	ListGroups(ctx context.Context) ([]identity.Group, error)
	AudienceMembers(ctx context.Context, groupID string) ([]identity.AudiencePerson, error)
	AudiencePerson(ctx context.Context, id uuid.UUID) (identity.AudiencePerson, error)
}

// AudienceAccounts reads core's accounts: a person being erased or erased
// (Blocked) gets no mail, and core's profile name comes first.
type AudienceAccounts interface {
	Accounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]user.Account, error)
}

// Via is how a recipient entered the audience.
type Via string

const (
	// ViaMember: as a Member (members), or as a member of a named team, of
	// a team below it, or of a team above it (Y1: a team's default
	// subscriber gets its sub-teams' mail).
	ViaMember Via = "member"
	// ViaFollower: only by following a named team or a team above it.
	ViaFollower Via = "follower"
)

// Recipient is one person a News send goes to.
type Recipient struct {
	UserID uuid.UUID `json:"userId"`
	// Email is the Primary e-mail, as Keycloak has it.
	Email string `json:"email"`
	Name  string `json:"name"`
	// Categories are the categories the person entered through that are on
	// for them; the unsubscribe link turns exactly these off.
	Categories []string `json:"categories"`
	Via        Via      `json:"via"`
	// Reason is the mail's "why you got this" line.
	Reason string `json:"reason"`
	// CategoryLabel names the categories for the mail's footer
	// ("AIRLAB · Duyurular").
	CategoryLabel  string `json:"categoryLabel"`
	UnsubscribeURL string `json:"unsubscribeUrl"`
}

// AudienceCounts are the recipients by how they entered.
type AudienceCounts struct {
	Members   int `json:"members"`
	Followers int `json:"followers"`
	Total     int `json:"total"`
}

// AudienceSkipped are the people left out, each counted once, under the
// first reason that held, in this order.
type AudienceSkipped struct {
	// NotVisible: the record is not visible to them (uyeler: no Member;
	// ekipler: outside the visibility teams, and every follower).
	NotVisible int `json:"notVisible"`
	// Disabled: disabled in Keycloak, gone from it, or being erased or
	// erased in core.
	Disabled int `json:"disabled"`
	// NoEmail: no Primary e-mail.
	NoEmail int `json:"noEmail"`
	// Unverified: Keycloak has not verified the Primary e-mail.
	Unverified int `json:"unverified"`
	// OptedOut: every category they entered through is off for them.
	OptedOut int `json:"optedOut"`
	// DuplicateEmail: another recipient (the lower user id) has the same
	// address, lower-cased.
	DuplicateEmail int `json:"duplicateEmail"`
}

// AudiencePage is one page of an audience.
type AudiencePage struct {
	Counts  AudienceCounts  `json:"counts"`
	Skipped AudienceSkipped `json:"skipped"`
	// Items are the recipients after the cursor, by user id; nil with
	// countOnly.
	Items []Recipient `json:"items,omitempty"`
	// Next is the cursor of the next page, uuid.Nil on the last.
	Next uuid.UUID `json:"-"`
}

// Audience resolves the audience of a News send (bilet 03,
// docs/news-audience.md): who gets it, why, and with which unsubscribe
// link, from the Keycloak tree of the moment and the preferences.
type Audience struct {
	prefs    *Service
	dir      AudienceDirectory
	accounts AudienceAccounts
	private  bool
	now      func() time.Time

	mu      sync.Mutex
	cached  *audienceSnapshot
	takenAt time.Time
	reads   singleflight.Group
}

// NewAudience resolves audiences over prefs' preferences and link key.
// private allows the uyeler and ekipler visibilities (PrivateVisibilityEnv).
func NewAudience(prefs *Service, dir AudienceDirectory, accounts AudienceAccounts, private bool) *Audience {
	return &Audience{prefs: prefs, dir: dir, accounts: accounts, private: private, now: time.Now}
}

// SetClock replaces the clock (tests).
func (a *Audience) SetClock(now func() time.Time) { a.now = now }

// Enabled reports whether audiences can be resolved: the preferences are on
// (every recipient needs a signed link).
func (a *Audience) Enabled() bool {
	return a != nil && a.prefs.Enabled() && a.dir != nil && a.accounts != nil
}

// PrivateVisibility reports whether uyeler and ekipler are accepted.
func (a *Audience) PrivateVisibility() bool { return a != nil && a.private }

// audienceSnapshot is the team tree and every person in a /UYELER Group,
// with the /UYELER Group paths they are directly in.
type audienceSnapshot struct {
	tree   *Tree
	people map[uuid.UUID]identity.AudiencePerson
	paths  map[uuid.UUID][]string
}

func (a *Audience) snapshot(ctx context.Context) (*audienceSnapshot, error) {
	now := a.now()
	a.mu.Lock()
	if a.cached != nil && now.Sub(a.takenAt) < AudienceSnapshotTTL {
		snap := a.cached
		a.mu.Unlock()
		return snap, nil
	}
	a.mu.Unlock()
	read, err, _ := a.reads.Do("snapshot", func() (any, error) {
		snap, err := a.readSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.cached, a.takenAt = snap, now
		a.mu.Unlock()
		return snap, nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTeamsUnavailable, err)
	}
	return read.(*audienceSnapshot), nil
}

var errNoMembersRoot = errors.New("the Group list has no " + authz.MembersRoot)

func (a *Audience) readSnapshot(ctx context.Context) (*audienceSnapshot, error) {
	groups, err := a.dir.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	var club []identity.Group
	rooted := false
	for _, g := range groups {
		if g.Path == authz.MembersRoot || strings.HasPrefix(g.Path, authz.MembersRoot+"/") {
			club = append(club, g)
			rooted = rooted || g.Path == authz.MembersRoot
		}
	}
	if !rooted {
		return nil, errNoMembersRoot
	}
	members := make([][]identity.AudiencePerson, len(club))
	reads, readCtx := errgroup.WithContext(ctx)
	reads.SetLimit(audienceReaders)
	for i, g := range club {
		reads.Go(func() error {
			people, err := a.dir.AudienceMembers(readCtx, g.ID)
			if errors.Is(err, identity.ErrNotFound) {
				// Deleted since the list was read: it holds nobody.
				return nil
			}
			members[i] = people
			return err
		})
	}
	if err := reads.Wait(); err != nil {
		return nil, err
	}
	snap := &audienceSnapshot{
		tree:   NewTree(groups, a.prefs.roots),
		people: map[uuid.UUID]identity.AudiencePerson{},
		paths:  map[uuid.UUID][]string{},
	}
	for i, g := range club {
		for _, p := range members[i] {
			snap.people[p.ID] = p
			snap.paths[p.ID] = append(snap.paths[p.ID], g.Path)
		}
	}
	return snap, nil
}

// teamRef finds a team by its Group path or id.
func (t *Tree) teamRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if _, ok := t.teams[ref]; ok {
		return ref, true
	}
	if id, ok := t.byPath[ref]; ok {
		return id, true
	}
	return "", false
}

func (t *Tree) teamRefs(refs []string) ([]string, error) {
	var out []string
	for _, ref := range refs {
		id, ok := t.teamRef(ref)
		if !ok {
			return nil, ErrUnknownTeam
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

func (r AudienceRequest) check(private bool) (Visibility, error) {
	if !r.Kind.valid() {
		return "", ErrInvalid
	}
	switch r.Type {
	case AudienceMembers:
		if len(r.Teams) != 0 {
			return "", ErrInvalid
		}
	case AudienceTeams:
		if len(r.Teams) == 0 || len(r.Teams) > MaxAudienceTeams {
			return "", ErrInvalid
		}
	default:
		return "", ErrInvalid
	}
	visibility := r.Visibility
	if visibility == "" {
		visibility = VisibilityPublic
	}
	switch visibility {
	case VisibilityPublic, VisibilityMembers:
		if len(r.VisibilityTeams) != 0 {
			return "", ErrInvalid
		}
	case VisibilityTeams:
		if len(r.VisibilityTeams) == 0 || len(r.VisibilityTeams) > MaxAudienceTeams {
			return "", ErrInvalid
		}
	default:
		return "", ErrInvalid
	}
	if visibility != VisibilityPublic && !private {
		return "", ErrVisibilityNotEnabled
	}
	return visibility, nil
}

// candidate is a person who may get the send, before the filters.
type candidate struct {
	person identity.AudiencePerson
	// found is false for a follower Keycloak no longer has.
	found      bool
	membership Membership
}

// Resolve answers one page of the audience of req: the recipients after
// the user id after (uuid.Nil: from the start), at most limit, by user id;
// countOnly answers the counts alone. Every page recomputes the whole
// audience on the snapshot of the moment, so its counts are the whole
// audience's.
func (a *Audience) Resolve(ctx context.Context, req AudienceRequest, after uuid.UUID, limit int, countOnly bool) (AudiencePage, error) {
	if !a.Enabled() {
		return AudiencePage{}, ErrDisabled
	}
	visibility, err := req.check(a.private)
	if err != nil {
		return AudiencePage{}, err
	}
	if limit <= 0 || limit > MaxAudiencePage {
		limit = MaxAudiencePage
	}
	snap, err := a.snapshot(ctx)
	if err != nil {
		return AudiencePage{}, err
	}
	tree := snap.tree
	targets, err := tree.teamRefs(req.Teams)
	if err != nil {
		return AudiencePage{}, err
	}
	visibleTeams, err := tree.teamRefs(req.VisibilityTeams)
	if err != nil {
		return AudiencePage{}, err
	}

	// The candidates: the Members, or everyone in a named team's chain
	// (the team, a team below it, a team above it), and everyone with a
	// choice on a team of a chain.
	candidates := map[uuid.UUID]*candidate{}
	for id, p := range snap.people {
		m := tree.Membership(snap.paths[id])
		in := false
		switch req.Type {
		case AudienceMembers:
			in = m.Member
		case AudienceTeams:
			for _, target := range targets {
				in = in || memberVia(tree, m, target) != ""
			}
		}
		if in {
			candidates[id] = &candidate{person: p, found: true, membership: m}
		}
	}
	if req.Type == AudienceTeams {
		var chains []string
		for _, target := range targets {
			for _, link := range tree.chain(target) {
				if !slices.Contains(chains, link) {
					chains = append(chains, link)
				}
			}
		}
		choosers, err := a.prefs.store.TeamChoosers(ctx, ChannelEmail, req.Kind, chains)
		if err != nil {
			return AudiencePage{}, err
		}
		for _, id := range choosers {
			if candidates[id] != nil {
				continue
			}
			if p, ok := snap.people[id]; ok {
				m := tree.Membership(snap.paths[id])
				if seesAny(tree, m, targets) {
					candidates[id] = &candidate{person: p, found: true, membership: m}
				}
				continue
			}
			// Not in any /UYELER Group: no Member, no team.
			if !seesAny(tree, Membership{}, targets) {
				continue
			}
			p, err := a.dir.AudiencePerson(ctx, id)
			switch {
			case errors.Is(err, identity.ErrNotFound):
				candidates[id] = &candidate{person: identity.AudiencePerson{Person: identity.Person{ID: id}}}
			case err != nil:
				return AudiencePage{}, fmt.Errorf("%w: %v", ErrTeamsUnavailable, err)
			default:
				candidates[id] = &candidate{person: p, found: true, membership: tree.Membership(nil)}
			}
		}
	}

	ids := make([]uuid.UUID, 0, len(candidates))
	for id, c := range candidates {
		// A client's service account is no person: never a recipient, not
		// counted.
		if c.person.ServiceAccount {
			continue
		}
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
	accounts, err := a.accounts.Accounts(ctx, ids)
	if err != nil {
		return AudiencePage{}, err
	}
	settings, err := a.prefs.store.ListPeople(ctx, ids)
	if err != nil {
		return AudiencePage{}, err
	}

	var page AudiencePage
	var recipients []Recipient
	addresses := map[string]bool{}
	for _, id := range ids {
		c := candidates[id]
		m := c.membership
		member := req.Type == AudienceMembers
		for _, target := range targets {
			member = member || memberVia(tree, m, target) != ""
		}
		switch {
		case visibility == VisibilityMembers && !m.Member,
			visibility == VisibilityTeams && (!member || !inAny(m, visibleTeams)):
			page.Skipped.NotVisible++
			continue
		}
		account, stored := accounts[id]
		email := strings.TrimSpace(c.person.Email)
		switch {
		case !c.found || !c.person.Enabled || id == user.DeletedSubject || (stored && account.Blocked):
			page.Skipped.Disabled++
			continue
		case email == "":
			page.Skipped.NoEmail++
			continue
		case !c.person.EmailVerified:
			page.Skipped.Unverified++
			continue
		}
		r, ok := a.recipient(tree, req, targets, visibility, c, ExplicitOf(settings[id], ChannelEmail))
		if !ok {
			page.Skipped.OptedOut++
			continue
		}
		address := strings.ToLower(email)
		if addresses[address] {
			page.Skipped.DuplicateEmail++
			continue
		}
		addresses[address] = true
		r.UserID, r.Email, r.Name = id, email, recipientName(c.person.Person, account)
		recipients = append(recipients, r)
		if r.Via == ViaMember {
			page.Counts.Members++
		} else {
			page.Counts.Followers++
		}
	}
	page.Counts.Total = len(recipients)
	if countOnly {
		return page, nil
	}
	start := 0
	if after != uuid.Nil {
		start, _ = slices.BinarySearchFunc(recipients, after, func(r Recipient, id uuid.UUID) int {
			return strings.Compare(r.UserID.String(), id.String())
		})
		if start < len(recipients) && recipients[start].UserID == after {
			start++
		}
	}
	end := min(start+limit, len(recipients))
	page.Items = make([]Recipient, 0, end-start)
	for _, r := range recipients[start:end] {
		cats := make([]Category, 0, len(r.Categories))
		for _, raw := range r.Categories {
			cat, err := ParseCategory(raw)
			if err != nil {
				return AudiencePage{}, err
			}
			cats = append(cats, cat)
		}
		link, err := a.prefs.UnsubscribeURL(r.UserID, ChannelEmail, cats)
		if err != nil {
			return AudiencePage{}, err
		}
		r.UnsubscribeURL = link
		page.Items = append(page.Items, r)
	}
	if end < len(recipients) {
		page.Next = recipients[end-1].UserID
	}
	return page, nil
}

// memberVia is the team of target's chain (target or a team above it) the
// person is in, nearest first; "" when none. A member of a team below
// target is in target (Membership marks every team above a person's own).
// A team above counts only when the person may see target: a hidden team
// reaches its own members alone (O2).
func memberVia(tree *Tree, m Membership, target string) string {
	if !tree.seen(m, target) {
		return ""
	}
	for _, link := range tree.chain(target) {
		if m.Teams[link] {
			return link
		}
	}
	return ""
}

// seesAny reports whether a person with membership m may see one of the
// teams: a choice on a hidden team's chain never makes a follower of it.
func seesAny(tree *Tree, m Membership, teams []string) bool {
	for _, id := range teams {
		if tree.seen(m, id) {
			return true
		}
	}
	return false
}

func inAny(m Membership, teams []string) bool {
	for _, id := range teams {
		if m.Teams[id] {
			return true
		}
	}
	return false
}

// recipient is the person's categories, how they entered and why, or false
// when every category they entered through is off. An ekipler record goes
// through membership only: a follower never gets it (Karar D7).
func (a *Audience) recipient(tree *Tree, req AudienceRequest, targets []string, visibility Visibility, c *candidate, e Explicit) (Recipient, bool) {
	m := c.membership
	var r Recipient
	if req.Type == AudienceMembers {
		cat := Club(req.Kind)
		if !tree.Resolve(e, m, cat).Effective {
			return Recipient{}, false
		}
		r.Categories = []string{cat.String()}
		r.Via, r.Reason = ViaMember, "SKY LAB üyesi olduğun için"
		r.CategoryLabel = "Kulüp geneli · " + kindLabel(req.Kind)
		return r, true
	}
	type entry struct {
		team, reason string
		member       bool
	}
	var entries []entry
	for _, target := range targets {
		cat := Team(target, req.Kind)
		v := tree.Resolve(e, m, cat)
		if !v.Effective {
			continue
		}
		if via := memberVia(tree, m, target); via != "" {
			entries = append(entries, entry{team: target, member: true, reason: tree.teams[via].label() + " ekibinde olduğun için"})
			continue
		}
		if visibility == VisibilityTeams {
			continue
		}
		followed := target
		if v.From != "" {
			followed = v.From
		}
		entries = append(entries, entry{team: target, reason: tree.teams[followed].label() + " ekibini takip ettiğin için"})
	}
	if len(entries) == 0 {
		return Recipient{}, false
	}
	// The reason of a membership comes first.
	slices.SortStableFunc(entries, func(x, y entry) int {
		switch {
		case x.member == y.member:
			return 0
		case x.member:
			return -1
		}
		return 1
	})
	var sources []string
	for _, en := range entries {
		r.Categories = append(r.Categories, Team(en.team, req.Kind).String())
		sources = append(sources, tree.teams[en.team].label())
	}
	r.Via, r.Reason = ViaFollower, entries[0].reason
	if entries[0].member {
		r.Via = ViaMember
	}
	r.CategoryLabel = strings.Join(sources, ", ") + " · " + kindLabel(req.Kind)
	return r, true
}

// label is how a News mail names the team: "KOD · display_name_tr", the
// code alone without a Turkish name (the membership mail's GroupName,
// teammail).
func (n *TeamNode) label() string {
	if n.DisplayName != "" && n.DisplayName != n.Code {
		return n.Code + " · " + n.DisplayName
	}
	return n.Code
}

// recipientName is core's profile name, Keycloak's when core has none.
func recipientName(p identity.Person, account user.Account) string {
	if account.Stored {
		if name := strings.TrimSpace(strings.TrimSpace(account.User.FirstName) + " " + strings.TrimSpace(account.User.LastName)); name != "" {
			return name
		}
	}
	return strings.TrimSpace(strings.TrimSpace(p.FirstName) + " " + strings.TrimSpace(p.LastName))
}
