package identity

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Service interface {
	ListGroups(ctx context.Context, p authz.Principal) ([]Group, error)
	Members(ctx context.Context, p authz.Principal, groupRef string) ([]Person, error)
	AddMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	RemoveMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	CreateUser(ctx context.Context, p authz.Principal, in Person) (Person, error)
	DeleteUser(ctx context.Context, p authz.Principal, id uuid.UUID) error
	ListPublicTeams(ctx context.Context) ([]PublicTeam, error)
	PublicMembers(ctx context.Context, team string) (Roster, error)
	PublicLeaders(ctx context.Context, team string) (Roster, error)
}

type service struct {
	dir   Directory
	users user.Store
	authz authz.Authorizer
}

func NewService(dir Directory, users user.Store, az authz.Authorizer) Service {
	return &service{dir: dir, users: users, authz: az}
}

func (s *service) allow(p authz.Principal, t authz.Type, a authz.Action) error {
	if !s.authz.Allow(p, authz.Resource{Type: t}, a) {
		return ErrForbidden
	}
	return nil
}

func (s *service) ListGroups(ctx context.Context, p authz.Principal) ([]Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return nil, err
	}
	return s.dir.ListGroups(ctx)
}

func (s *service) Members(ctx context.Context, p authz.Principal, groupRef string) ([]Person, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return nil, err
	}
	return s.dir.Members(ctx, groupRef)
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
	return s.dir.RemoveMember(ctx, groupRef, userID)
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
	_, err = s.users.Upsert(ctx, user.User{
		ID:        created.ID,
		Email:     created.Email,
		FirstName: created.FirstName,
		LastName:  created.LastName,
	})
	if err != nil {
		_ = s.dir.DeleteUser(ctx, created.ID)
		return Person{}, err
	}
	return created, nil
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
	return buildRoster(g, people, leaders), nil
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
	return buildRoster(g, people, leaders), nil
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

func (s *service) collectMembers(ctx context.Context, root Group, recursive bool) ([]Person, error) {
	seen := make(map[uuid.UUID]struct{})
	var out []Person
	groups := []Group{root}
	if recursive {
		desc, err := s.descendants(ctx, root)
		if err != nil {
			return nil, err
		}
		groups = append(groups, desc...)
	}
	for _, g := range groups {
		members, err := s.dir.Members(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range members {
			if _, ok := seen[p.ID]; ok {
				continue
			}
			seen[p.ID] = struct{}{}
			out = append(out, p)
		}
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

func buildRoster(g Group, people []Person, leaders map[uuid.UUID]struct{}) Roster {
	members := make([]PublicMember, 0, len(people))
	for _, p := range people {
		_, leader := leaders[p.ID]
		members = append(members, PublicMember{
			FirstName: p.FirstName,
			LastName:  p.LastName,
			Leader:    leader,
		})
	}
	return Roster{
		Team:        g.Name,
		DisplayName: displayName(g),
		Description: description(g),
		Count:       len(members),
		Members:     members,
	}
}
