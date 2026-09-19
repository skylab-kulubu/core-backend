package identity

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

type Memory struct {
	mu         sync.Mutex
	groups     map[string]Group
	people     map[uuid.UUID]Person
	members    map[string]map[uuid.UUID]struct{}
	groupRoles map[string][]ClientRole
	userRoles  map[uuid.UUID][]ClientRole
	catalog    []ClientRole
	Ops        []string
}

func NewMemory() *Memory {
	return &Memory{
		groups:     make(map[string]Group),
		people:     make(map[uuid.UUID]Person),
		members:    make(map[string]map[uuid.UUID]struct{}),
		groupRoles: make(map[string][]ClientRole),
		userRoles:  make(map[uuid.UUID][]ClientRole),
	}
}

func (m *Memory) PutClientRole(role ClientRole) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.catalog = append(m.catalog, role)
}

func (m *Memory) PutGroup(g Group) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.groups[g.ID] = g
}

func (m *Memory) PutUser(p Person) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	m.people[p.ID] = p
}

func (m *Memory) record(op string) {
	m.Ops = append(m.Ops, op)
}

func (m *Memory) resolveLocked(idOrPath string) (Group, bool) {
	if g, ok := m.groups[idOrPath]; ok {
		return g, true
	}
	for _, g := range m.groups {
		if g.Path == idOrPath || g.Name == idOrPath {
			return g, true
		}
	}
	return Group{}, false
}

func (m *Memory) ListGroups(_ context.Context) ([]Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("ListGroups")
	out := make([]Group, 0, len(m.groups))
	for _, g := range m.groups {
		out = append(out, g)
	}
	return out, nil
}

func (m *Memory) GetGroup(_ context.Context, idOrPath string) (Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("GetGroup")
	g, ok := m.resolveLocked(idOrPath)
	if !ok {
		return Group{}, ErrNotFound
	}
	return g, nil
}

func (m *Memory) UpdateGroup(_ context.Context, g Group) (Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("UpdateGroup")
	existing, ok := m.resolveLocked(g.ID)
	if !ok {
		existing, ok = m.resolveLocked(g.Path)
	}
	if !ok {
		return Group{}, ErrNotFound
	}
	if g.Name != "" {
		existing.Name = g.Name
	}
	if g.Attributes != nil {
		existing.Attributes = g.Attributes
	}
	m.groups[existing.ID] = existing
	return existing, nil
}

func (m *Memory) CreateGroup(_ context.Context, parentRef, name string) (Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("CreateGroup")
	path := "/" + name
	if parentRef != "" {
		parent, ok := m.resolveLocked(parentRef)
		if !ok {
			return Group{}, ErrNotFound
		}
		path = parent.Path + "/" + name
	}
	g := Group{ID: uuid.New().String(), Name: name, Path: path}
	m.groups[g.ID] = g
	return g, nil
}

func (m *Memory) Subgroups(_ context.Context, groupID string) ([]Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("Subgroups")
	g, ok := m.resolveLocked(groupID)
	if !ok {
		return nil, ErrNotFound
	}
	prefix := g.Path + "/"
	out := make([]Group, 0)
	for _, child := range m.groups {
		if !strings.HasPrefix(child.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(child.Path, prefix)
		if rest != "" && !strings.Contains(rest, "/") {
			out = append(out, child)
		}
	}
	return out, nil
}

func (m *Memory) Members(_ context.Context, groupID string) ([]Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("Members")
	g, ok := m.resolveLocked(groupID)
	if !ok {
		return nil, ErrNotFound
	}
	ids := m.members[g.ID]
	out := make([]Person, 0, len(ids))
	for id := range ids {
		if p, ok := m.people[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *Memory) AddMember(_ context.Context, groupID string, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("AddMember")
	g, ok := m.resolveLocked(groupID)
	if !ok {
		return ErrNotFound
	}
	if _, ok := m.people[userID]; !ok {
		return ErrNotFound
	}
	if m.members[g.ID] == nil {
		m.members[g.ID] = make(map[uuid.UUID]struct{})
	}
	m.members[g.ID][userID] = struct{}{}
	return nil
}

func (m *Memory) RemoveMember(_ context.Context, groupID string, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("RemoveMember")
	g, ok := m.resolveLocked(groupID)
	if !ok {
		return ErrNotFound
	}
	delete(m.members[g.ID], userID)
	return nil
}

func (m *Memory) ListClientRoles(_ context.Context) ([]ClientRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("ListClientRoles")
	seen := map[string]struct{}{}
	out := make([]ClientRole, 0, len(m.catalog))
	add := func(role ClientRole) {
		key := role.ClientID + "\x00" + role.Role
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, role)
	}
	for _, role := range m.catalog {
		add(role)
	}
	for _, roles := range m.groupRoles {
		for _, role := range roles {
			add(role)
		}
	}
	for _, roles := range m.userRoles {
		for _, role := range roles {
			add(role)
		}
	}
	return out, nil
}

func (m *Memory) ListUsers(_ context.Context) ([]Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("ListUsers")
	out := make([]Person, 0, len(m.people))
	for _, p := range m.people {
		out = append(out, p)
	}
	return out, nil
}

func (m *Memory) SearchUsers(_ context.Context, query string, limit int) ([]Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("SearchUsers")
	out := make([]Person, 0)
	for _, person := range m.people {
		if personMatches(person, strings.TrimSpace(query)) {
			out = append(out, person)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		left := strings.ToLower(out[i].LastName + "\x00" + out[i].FirstName + "\x00" + out[i].Email)
		right := strings.ToLower(out[j].LastName + "\x00" + out[j].FirstName + "\x00" + out[j].Email)
		return left < right
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func holdsClientRole(have ClientRole, clientID, role string) bool {
	if have.ClientID != clientID && !((clientID == "forms" || clientID == "dotnet") && have.ClientID == "skyforms") {
		return false
	}
	if have.Role == role {
		return true
	}
	return strings.HasPrefix(role, "skyforms:") && have.Role == "skyforms:*"
}

func (m *Memory) UsersWithClientRole(_ context.Context, clientID, role string) ([]Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("UsersWithClientRole")
	seen := map[uuid.UUID]struct{}{}
	out := make([]Person, 0)
	add := func(id uuid.UUID) {
		if _, ok := seen[id]; ok {
			return
		}
		p, ok := m.people[id]
		if !ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, p)
	}
	for gid, roles := range m.groupRoles {
		ok := false
		for _, r := range roles {
			if holdsClientRole(r, clientID, role) {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		for id := range m.members[gid] {
			add(id)
		}
	}
	for id, roles := range m.userRoles {
		for _, r := range roles {
			if holdsClientRole(r, clientID, role) {
				add(id)
				break
			}
		}
	}
	return out, nil
}

func (m *Memory) CreateUser(_ context.Context, p Person) (Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("CreateUser")
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.Username == "" {
		p.Username = p.Email
	}
	m.people[p.ID] = p
	return p, nil
}

func (m *Memory) GetUser(_ context.Context, id uuid.UUID) (Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("GetUser")
	p, ok := m.people[id]
	if !ok {
		return Person{}, ErrNotFound
	}
	return p, nil
}

func (m *Memory) DeleteUser(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("DeleteUser")
	if _, ok := m.people[id]; !ok {
		return ErrNotFound
	}
	delete(m.people, id)
	delete(m.userRoles, id)
	for _, members := range m.members {
		delete(members, id)
	}
	return nil
}

func (m *Memory) GroupsForUser(_ context.Context, userID uuid.UUID) ([]Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("GroupsForUser")
	out := make([]Group, 0)
	for gid, members := range m.members {
		if _, ok := members[userID]; !ok {
			continue
		}
		if g, ok := m.groups[gid]; ok {
			out = append(out, g)
		}
	}
	return out, nil
}

func (m *Memory) GroupClientRoles(_ context.Context, groupID string) ([]ClientRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("GroupClientRoles")
	g, ok := m.resolveLocked(groupID)
	if !ok {
		return nil, ErrNotFound
	}
	roles := m.groupRoles[g.ID]
	out := make([]ClientRole, len(roles))
	copy(out, roles)
	return out, nil
}

func (m *Memory) SetGroupClientRoles(_ context.Context, groupID string, roles []ClientRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("SetGroupClientRoles")
	g, ok := m.resolveLocked(groupID)
	if !ok {
		return ErrNotFound
	}
	cp := make([]ClientRole, len(roles))
	copy(cp, roles)
	m.groupRoles[g.ID] = cp
	return nil
}

func (m *Memory) UserExtraRoles(_ context.Context, userID uuid.UUID) ([]ClientRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("UserExtraRoles")
	roles := m.userRoles[userID]
	out := make([]ClientRole, len(roles))
	copy(out, roles)
	return out, nil
}

func (m *Memory) AddUserExtraRole(_ context.Context, userID uuid.UUID, role ClientRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("AddUserExtraRole")
	if _, ok := m.people[userID]; !ok {
		return ErrNotFound
	}
	for _, existing := range m.userRoles[userID] {
		if existing.ClientID == role.ClientID && existing.Role == role.Role {
			return nil
		}
	}
	m.userRoles[userID] = append(m.userRoles[userID], role)
	return nil
}

func (m *Memory) RemoveUserExtraRole(_ context.Context, userID uuid.UUID, role ClientRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("RemoveUserExtraRole")
	kept := make([]ClientRole, 0)
	for _, existing := range m.userRoles[userID] {
		if existing.ClientID == role.ClientID && existing.Role == role.Role {
			continue
		}
		kept = append(kept, existing)
	}
	m.userRoles[userID] = kept
	return nil
}

func (m *Memory) LogoutAllSessions(_ context.Context, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("LogoutAllSessions")
	if _, ok := m.people[userID]; !ok {
		return ErrNotFound
	}
	return nil
}

func (m *Memory) WriteSkyNumber(_ context.Context, userID uuid.UUID, skyNumber string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("WriteSkyNumber")
	p, ok := m.people[userID]
	if !ok {
		return ErrNotFound
	}
	p.SkyNumber = skyNumber
	m.people[userID] = p
	return nil
}

func (m *Memory) ReadSkyNumber(_ context.Context, userID uuid.UUID) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.people[userID]
	if !ok {
		return "", ErrNotFound
	}
	return p.SkyNumber, nil
}
