package identity

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Service interface {
	ListGroups(ctx context.Context, p authz.Principal) ([]Group, error)
	GetGroup(ctx context.Context, p authz.Principal, groupRef string) (Group, error)
	CreateGroup(ctx context.Context, p authz.Principal, parentRef, name string) (Group, error)
	UpdateGroup(ctx context.Context, p authz.Principal, groupRef, name string, attrs map[string]string) (Group, error)
	Members(ctx context.Context, p authz.Principal, groupRef string) ([]GroupMember, error)
	AddMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	RemoveMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	ListUsers(ctx context.Context, p authz.Principal, q string, seat ...ClientRole) ([]Person, error)
	ListClientRoles(ctx context.Context, p authz.Principal) ([]ClientRole, error)
	GetUser(ctx context.Context, p authz.Principal, id uuid.UUID) (UserCard, error)
	PatchUser(ctx context.Context, p authz.Principal, id uuid.UUID, in user.ProfilePatch) (UserCard, error)
	CreateUser(ctx context.Context, p authz.Principal, in Person) (Person, error)
	DeleteUser(ctx context.Context, p authz.Principal, id uuid.UUID) error
	GroupClientRoles(ctx context.Context, p authz.Principal, groupRef string) ([]ClientRole, error)
	SetGroupClientRoles(ctx context.Context, p authz.Principal, groupRef string, roles []ClientRole) error
	AddUserExtraRole(ctx context.Context, p authz.Principal, id uuid.UUID, role ClientRole) error
	RemoveUserExtraRole(ctx context.Context, p authz.Principal, id uuid.UUID, role ClientRole) error
	LogoutAllSessions(ctx context.Context, p authz.Principal, id uuid.UUID) error
	ListPublicTeams(ctx context.Context) ([]PublicTeam, error)
	PublicMembers(ctx context.Context, team string) (Roster, error)
	PublicLeaders(ctx context.Context, team string) (Roster, error)
}

type service struct {
	dir    Directory
	users  user.Store
	assign user.Service
	authz  authz.Authorizer
	mail   mail.Mailer
}

func NewService(dir Directory, users user.Store, az authz.Authorizer, mailers ...mail.Mailer) Service {
	s := &service{dir: dir, users: users, assign: user.NewService(users, dir), authz: az}
	if len(mailers) > 0 {
		s.mail = mailers[0]
	}
	return s
}

func (s *service) allow(p authz.Principal, t authz.Type, a authz.Action) error {
	if !s.authz.Allow(p, authz.Resource{Type: t}, a) {
		return ErrForbidden
	}
	return nil
}

func (s *service) allowUserRead(p authz.Principal) error {
	if s.allow(p, authz.TypeUser, authz.Read) == nil {
		return nil
	}
	for _, role := range p.Roles {
		if role == "users:read" {
			return nil
		}
	}
	return ErrForbidden
}

func (s *service) ListGroups(ctx context.Context, p authz.Principal) ([]Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return nil, err
	}
	return s.dir.ListGroups(ctx)
}

func (s *service) GetGroup(ctx context.Context, p authz.Principal, groupRef string) (Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return Group{}, err
	}
	return s.dir.GetGroup(ctx, groupRef)
}

func (s *service) CreateGroup(ctx context.Context, p authz.Principal, parentRef, name string) (Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Create); err != nil {
		return Group{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, ErrInvalid
	}
	return s.dir.CreateGroup(ctx, parentRef, name)
}

func (s *service) UpdateGroup(ctx context.Context, p authz.Principal, groupRef, name string, attrs map[string]string) (Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return Group{}, err
	}
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return Group{}, err
	}
	if name != "" {
		g.Name = name
	}
	if attrs != nil {
		g.Attributes = attrs
	}
	return s.dir.UpdateGroup(ctx, g)
}

func (s *service) Members(ctx context.Context, p authz.Principal, groupRef string) ([]GroupMember, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return nil, err
	}
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return nil, err
	}
	hits, err := s.memberHits(ctx, g)
	if err != nil {
		return nil, err
	}
	return rosterMembers(g, hits), nil
}

func (s *service) AddMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return err
	}
	return s.dir.AddMember(ctx, groupRef, userID)
}

func (s *service) RemoveMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return err
	}
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return err
	}
	hits, err := s.memberHits(ctx, g)
	if err != nil {
		return err
	}
	for _, h := range hits {
		if h.person.ID == userID {
			return s.dir.RemoveMember(ctx, h.source.ID, userID)
		}
	}
	return s.dir.RemoveMember(ctx, g.ID, userID)
}

func (s *service) ListClientRoles(ctx context.Context, p authz.Principal) ([]ClientRole, error) {
	if err := s.allow(p, authz.TypeUser, authz.Read); err != nil {
		return nil, err
	}
	roles, err := s.dir.ListClientRoles(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(roles, func(a, b ClientRole) int {
		if a.ClientID != b.ClientID {
			return strings.Compare(a.ClientID, b.ClientID)
		}
		return strings.Compare(a.Role, b.Role)
	})
	return roles, nil
}

func (s *service) ListUsers(ctx context.Context, p authz.Principal, q string, seat ...ClientRole) ([]Person, error) {
	if err := s.allow(p, authz.TypeUser, authz.Read); err != nil {
		return nil, err
	}
	q = strings.TrimSpace(q)
	if len(seat) > 0 && strings.TrimSpace(seat[0].Role) != "" {
		clientID := strings.TrimSpace(seat[0].ClientID)
		if clientID == "" {
			clientID = "forms"
		}
		people, err := s.dir.UsersWithClientRole(ctx, clientID, strings.TrimSpace(seat[0].Role))
		if err != nil {
			return nil, err
		}
		if q != "" {
			matched := make([]Person, 0, len(people))
			for _, person := range people {
				if personMatches(person, q) {
					matched = append(matched, person)
				}
			}
			people = matched
		}
		return s.overlayShadow(ctx, people)
	}
	people, err := s.dir.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	if q != "" {
		return s.mergeUserSearch(ctx, people, q)
	}
	out := make([]Person, 0, len(people))
	for _, person := range people {
		shadow, _, err := s.assign.Ensure(ctx, person.ID, user.Profile{
			Email:       person.Email,
			FirstName:   person.FirstName,
			LastName:    person.LastName,
			Username:    person.Username,
			SchoolEmail: person.SchoolEmail,
			SkyNumber:   person.SkyNumber,
		})
		if err != nil {
			if errors.Is(err, user.ErrConflict) {
				out = append(out, person)
				continue
			}
			return nil, err
		}
		if shadow.ID == person.ID {
			if shadow.SchoolEmail != "" {
				person.SchoolEmail = shadow.SchoolEmail
			}
			if shadow.SkyNumber != "" {
				person.SkyNumber = shadow.SkyNumber
			}
		}
		out = append(out, person)
	}
	return out, nil
}

func (s *service) GetUser(ctx context.Context, p authz.Principal, id uuid.UUID) (UserCard, error) {
	if err := s.allowUserRead(p); err != nil {
		return UserCard{}, err
	}
	person, err := s.dir.GetUser(ctx, id)
	if err != nil {
		return UserCard{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.Update) {
		if got, err := s.users.Get(ctx, id); err == nil {
			if got.FirstName != "" {
				person.FirstName = got.FirstName
			}
			if got.LastName != "" {
				person.LastName = got.LastName
			}
		}
		return nameOnlyCard(UserCard{Person: Person{
			ID:        person.ID,
			Email:     person.Email,
			FirstName: person.FirstName,
			LastName:  person.LastName,
		}}), nil
	}
	var shadow user.User
	if got, err := s.users.Get(ctx, id); err == nil {
		person = overlayPerson(person, got)
		shadow = got
	}
	groups, err := s.dir.GroupsForUser(ctx, id)
	if err != nil {
		return UserCard{}, err
	}
	inherited := make([]ClientRole, 0)
	seen := map[string]struct{}{}
	for _, g := range groups {
		roles, err := s.dir.GroupClientRoles(ctx, g.ID)
		if err != nil {
			return UserCard{}, err
		}
		for _, r := range roles {
			key := r.ClientID + ":" + r.Role
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			inherited = append(inherited, r)
		}
	}
	extra, err := s.dir.UserExtraRoles(ctx, id)
	if err != nil {
		return UserCard{}, err
	}
	if groups == nil {
		groups = []Group{}
	}
	if extra == nil {
		extra = []ClientRole{}
	}
	return userCard(person, groups, inherited, extra, shadow), nil
}

func nameOnlyCard(card UserCard) UserCard {
	return UserCard{
		Person: Person{
			ID:        card.ID,
			Email:     card.Email,
			FirstName: card.FirstName,
			LastName:  card.LastName,
		},
	}
}

func overlayPerson(person Person, shadow user.User) Person {
	if shadow.SchoolEmail != "" {
		person.SchoolEmail = shadow.SchoolEmail
	}
	if shadow.SkyNumber != "" {
		person.SkyNumber = shadow.SkyNumber
	}
	if shadow.FirstName != "" {
		person.FirstName = shadow.FirstName
	}
	if shadow.LastName != "" {
		person.LastName = shadow.LastName
	}
	return person
}

func userCard(person Person, groups []Group, inherited, extra []ClientRole, shadow user.User) UserCard {
	return UserCard{
		Person:         person,
		Linkedin:       shadow.Linkedin,
		University:     shadow.University,
		Faculty:        shadow.Faculty,
		Department:     shadow.Department,
		Phone:          shadow.Phone,
		StudentCardUid: shadow.StudentCardUID,
		Groups:         groups,
		InheritedRoles: inherited,
		ExtraRoles:     extra,
	}
}

func (s *service) PatchUser(ctx context.Context, p authz.Principal, id uuid.UUID, in user.ProfilePatch) (UserCard, error) {
	if err := s.allow(p, authz.TypeUser, authz.Update); err != nil {
		return UserCard{}, err
	}
	person, err := s.dir.GetUser(ctx, id)
	if err != nil {
		return UserCard{}, err
	}
	if _, err := s.users.Get(ctx, id); errors.Is(err, user.ErrNotFound) {
		if _, _, err := s.assign.Ensure(ctx, id, user.Profile{
			Email:       person.Email,
			FirstName:   person.FirstName,
			LastName:    person.LastName,
			Username:    person.Username,
			SchoolEmail: person.SchoolEmail,
			SkyNumber:   person.SkyNumber,
		}); err != nil {
			return UserCard{}, err
		}
	} else if err != nil {
		return UserCard{}, err
	}
	if _, err := s.assign.Patch(ctx, id, in); err != nil {
		return UserCard{}, err
	}
	return s.GetUser(ctx, p, id)
}

func (s *service) GroupClientRoles(ctx context.Context, p authz.Principal, groupRef string) ([]ClientRole, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return nil, err
	}
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return nil, err
	}
	return s.dir.GroupClientRoles(ctx, g.ID)
}

func (s *service) SetGroupClientRoles(ctx context.Context, p authz.Principal, groupRef string, roles []ClientRole) error {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return err
	}
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return err
	}
	return s.dir.SetGroupClientRoles(ctx, g.ID, roles)
}

func (s *service) AddUserExtraRole(ctx context.Context, p authz.Principal, id uuid.UUID, role ClientRole) error {
	if err := s.allow(p, authz.TypeUser, authz.Update); err != nil {
		return err
	}
	if role.ClientID == "" || role.Role == "" {
		return ErrInvalid
	}
	if _, err := s.dir.GetUser(ctx, id); err != nil {
		return err
	}
	return s.dir.AddUserExtraRole(ctx, id, role)
}

func (s *service) RemoveUserExtraRole(ctx context.Context, p authz.Principal, id uuid.UUID, role ClientRole) error {
	if err := s.allow(p, authz.TypeUser, authz.Update); err != nil {
		return err
	}
	return s.dir.RemoveUserExtraRole(ctx, id, role)
}

func (s *service) CreateUser(ctx context.Context, p authz.Principal, in Person) (Person, error) {
	if err := s.allow(p, authz.TypeUser, authz.Create); err != nil {
		return Person{}, err
	}
	if in.Email == "" {
		return Person{}, ErrInvalid
	}
	created, err := s.dir.CreateUser(ctx, in)
	if err != nil {
		return Person{}, err
	}
	shadow, _, err := s.assign.Ensure(ctx, created.ID, user.Profile{
		Email:       created.Email,
		FirstName:   created.FirstName,
		LastName:    created.LastName,
		SchoolEmail: created.SchoolEmail,
		SkyNumber:   created.SkyNumber,
	})
	if err != nil {
		_ = s.dir.DeleteUser(ctx, created.ID)
		return Person{}, err
	}
	created.SkyNumber = shadow.SkyNumber
	if s.mail != nil {
		s.mail.Welcome(ctx, shadow)
	}
	return created, nil
}

func (s *service) LogoutAllSessions(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if err := s.allow(p, authz.TypeUser, authz.Update); err != nil {
		return err
	}
	if _, err := s.dir.GetUser(ctx, id); err != nil {
		return err
	}
	return s.dir.LogoutAllSessions(ctx, id)
}

func (s *service) DeleteUser(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if err := s.allow(p, authz.TypeUser, authz.Delete); err != nil {
		return err
	}
	if err := s.dir.DeleteUser(ctx, id); err != nil {
		return err
	}
	err := s.users.Delete(ctx, id)
	if err != nil && !errors.Is(err, user.ErrNotFound) {
		return err
	}
	return nil
}

func (s *service) ListPublicTeams(ctx context.Context) ([]PublicTeam, error) {
	if err := s.allow(authz.Principal{}, authz.TypeTeam, authz.Read); err != nil {
		return nil, err
	}
	groups, err := s.dir.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PublicTeam, 0)
	for _, g := range groups {
		if !attrTrue(g.Attributes, "public_listing") {
			continue
		}
		out = append(out, PublicTeam{
			Team:        g.Name,
			Path:        g.Path,
			DisplayName: displayName(g),
		})
	}
	return out, nil
}

func (s *service) PublicMembers(ctx context.Context, team string) (Roster, error) {
	g, err := s.publicGroup(ctx, team, false)
	if err != nil {
		return Roster{}, err
	}
	people, err := s.collectMembers(ctx, g, true)
	if err != nil {
		return Roster{}, err
	}
	leaders, err := s.leaderIDs(ctx, g)
	if err != nil {
		return Roster{}, err
	}
	return s.buildRoster(ctx, g, people, leaders), nil
}

func (s *service) PublicLeaders(ctx context.Context, team string) (Roster, error) {
	g, err := s.publicGroup(ctx, team, true)
	if err != nil {
		return Roster{}, err
	}
	leaders, err := s.leaderIDs(ctx, g)
	if err != nil {
		return Roster{}, err
	}
	people := make([]Person, 0, len(leaders))
	for id := range leaders {
		p, err := s.dir.GetUser(ctx, id)
		if err != nil {
			continue
		}
		people = append(people, p)
	}
	return s.buildRoster(ctx, g, people, leaders), nil
}

func (s *service) publicGroup(ctx context.Context, team string, leaders bool) (Group, error) {
	if err := s.allow(authz.Principal{}, authz.TypeTeam, authz.Read); err != nil {
		return Group{}, err
	}
	g, err := s.dir.GetGroup(ctx, team)
	if err != nil {
		return Group{}, ErrNotFound
	}
	if leaders {
		if !attrTrue(g.Attributes, "public_listing") && !attrTrue(g.Attributes, "public_leaders") {
			return Group{}, ErrNotFound
		}
		return g, nil
	}
	if !attrTrue(g.Attributes, "public_listing") {
		return Group{}, ErrNotFound
	}
	return g, nil
}

type memberHit struct {
	person Person
	source Group
}

func (s *service) memberHits(ctx context.Context, root Group) ([]memberHit, error) {
	groups := []Group{root}
	desc, err := s.descendants(ctx, root)
	if err != nil {
		return nil, err
	}
	groups = append(groups, desc...)
	slices.SortFunc(groups, func(a, b Group) int {
		da, db := strings.Count(a.Path, "/"), strings.Count(b.Path, "/")
		if da != db {
			return da - db
		}
		return strings.Compare(a.Path, b.Path)
	})
	best := make(map[uuid.UUID]memberHit)
	order := make([]uuid.UUID, 0)
	for _, g := range groups {
		members, err := s.dir.Members(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range members {
			if _, ok := best[p.ID]; ok {
				continue
			}
			best[p.ID] = memberHit{person: p, source: g}
			order = append(order, p.ID)
		}
	}
	out := make([]memberHit, 0, len(order))
	for _, id := range order {
		out = append(out, best[id])
	}
	return out, nil
}

func rosterMembers(root Group, hits []memberHit) []GroupMember {
	out := make([]GroupMember, 0, len(hits))
	for _, h := range hits {
		m := GroupMember{Person: h.person}
		if h.source.ID != root.ID {
			m.SourceGroupID = h.source.ID
			m.SourceGroupPath = h.source.Path
		}
		out = append(out, m)
	}
	return out
}

func (s *service) collectMembers(ctx context.Context, root Group, recursive bool) ([]Person, error) {
	if !recursive {
		return s.dir.Members(ctx, root.ID)
	}
	hits, err := s.memberHits(ctx, root)
	if err != nil {
		return nil, err
	}
	out := make([]Person, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.person)
	}
	return out, nil
}

func (s *service) descendants(ctx context.Context, root Group) ([]Group, error) {
	children, err := s.dir.Subgroups(ctx, root.ID)
	if err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(children))
	for _, child := range children {
		out = append(out, child)
		nested, err := s.descendants(ctx, child)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

func (s *service) leaderIDs(ctx context.Context, team Group) (map[uuid.UUID]struct{}, error) {
	children, err := s.dir.Subgroups(ctx, team.ID)
	if err != nil {
		return nil, err
	}
	ids := make(map[uuid.UUID]struct{})
	subs := authz.DefaultPolicy().LeaderSubgroups
	for _, child := range children {
		if !isLeaderSubgroup(child.Name, subs) {
			continue
		}
		members, err := s.dir.Members(ctx, child.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range members {
			ids[p.ID] = struct{}{}
		}
	}
	return ids, nil
}

func isLeaderSubgroup(name string, subs []string) bool {
	for _, sub := range subs {
		if name == sub {
			return true
		}
	}
	return false
}

func attrTrue(attrs map[string]string, key string) bool {
	if attrs == nil {
		return false
	}
	return strings.EqualFold(attrs[key], "true")
}

func displayName(g Group) LocalizedText {
	tr := g.Name
	en := ""
	if g.Attributes != nil {
		if v := g.Attributes["display_name_tr"]; v != "" {
			tr = v
		}
		en = g.Attributes["display_name_en"]
	}
	return LocalizedText{TR: tr, EN: en}
}

func description(g Group) *LocalizedText {
	if g.Attributes == nil {
		return nil
	}
	tr := g.Attributes["description_tr"]
	en := g.Attributes["description_en"]
	if tr == "" && en == "" {
		return nil
	}
	return &LocalizedText{TR: tr, EN: en}
}

func (s *service) buildRoster(ctx context.Context, g Group, people []Person, leaders map[uuid.UUID]struct{}) Roster {
	members := make([]PublicMember, 0, len(people))
	for _, p := range people {
		_, leader := leaders[p.ID]
		m := PublicMember{
			FirstName: p.FirstName,
			LastName:  p.LastName,
			Leader:    leader,
		}
		if shadow, err := s.users.Get(ctx, p.ID); err == nil {
			m.Linkedin = shadow.Linkedin
			m.University = shadow.University
			m.Faculty = shadow.Faculty
			m.Department = shadow.Department
			m.ProfilePictureURL = media.PublicURL("", shadow.ProfilePictureURL)
		}
		members = append(members, m)
	}
	return Roster{
		Team:        g.Name,
		DisplayName: displayName(g),
		Description: description(g),
		Count:       len(members),
		Members:     members,
	}
}

func (s *service) overlayShadow(ctx context.Context, people []Person) ([]Person, error) {
	for i, person := range people {
		shadow, err := s.users.Get(ctx, person.ID)
		if err != nil {
			continue
		}
		if shadow.SchoolEmail != "" {
			people[i].SchoolEmail = shadow.SchoolEmail
		}
		if shadow.SkyNumber != "" {
			people[i].SkyNumber = shadow.SkyNumber
		}
	}
	return people, nil
}

func (s *service) mergeUserSearch(ctx context.Context, people []Person, q string) ([]Person, error) {
	out := make([]Person, 0)
	seen := map[uuid.UUID]int{}
	for _, person := range people {
		if !personMatches(person, q) {
			continue
		}
		seen[person.ID] = len(out)
		out = append(out, person)
	}
	found, err := s.users.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, u := range found {
		person := personFromUser(u)
		if i, ok := seen[u.ID]; ok {
			if person.SchoolEmail != "" {
				out[i].SchoolEmail = person.SchoolEmail
			}
			if person.SkyNumber != "" {
				out[i].SkyNumber = person.SkyNumber
			}
			continue
		}
		seen[u.ID] = len(out)
		out = append(out, person)
	}
	return out, nil
}

func personMatches(p Person, q string) bool {
	n := strings.ToLower(q)
	for _, f := range []string{
		p.Email, p.FirstName, p.LastName, p.Username, p.SchoolEmail, p.SkyNumber,
		strings.TrimSpace(p.FirstName + " " + p.LastName),
	} {
		if strings.Contains(strings.ToLower(f), n) {
			return true
		}
	}
	return false
}

func personFromUser(u user.User) Person {
	return Person{
		ID:          u.ID,
		Email:       u.Email,
		FirstName:   u.FirstName,
		LastName:    u.LastName,
		Username:    u.Username,
		SchoolEmail: u.SchoolEmail,
		SkyNumber:   u.SkyNumber,
	}
}
