package identity

import (
	"context"
	"slices"
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
	// composites are the roles each role includes, by roleKey; a role with
	// an empty ClientID is a realm role. defaultRoles every person holds
	// (Keycloak's default-roles-<realm>).
	composites   map[string][]ClientRole
	defaultRoles []ClientRole
	// disabled are the people DisableUser disabled: Keycloak's
	// enabled=false. Everyone else is enabled, and PutUser enables again.
	disabled map[uuid.UUID]struct{}
	// parents are the parent ids SetGroupParent set; a Group without one
	// has the parent its path says.
	parents map[string]string
	// idpLinks are the people LinkIdentityProvider linked, by alias;
	// personalEmails the Personal e-mail and its proof stamp
	// SetPersonalEmail gave a person (address_directory.go).
	idpLinks       map[string]map[uuid.UUID]struct{}
	personalEmails map[uuid.UUID][2]string
	Ops            []string
}

func NewMemory() *Memory {
	return &Memory{
		groups:     make(map[string]Group),
		people:     make(map[uuid.UUID]Person),
		members:    make(map[string]map[uuid.UUID]struct{}),
		groupRoles: make(map[string][]ClientRole),
		userRoles:  make(map[uuid.UUID][]ClientRole),
		composites: make(map[string][]ClientRole),
		disabled:   make(map[uuid.UUID]struct{}),
		parents:    make(map[string]string),
	}
}

func roleKey(r ClientRole) string { return r.ClientID + "\x00" + r.Role }

// PutCompositeRole makes parent include children, as a Keycloak composite
// role does. A role with an empty ClientID is a realm role.
func (m *Memory) PutCompositeRole(parent ClientRole, children ...ClientRole) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.composites[roleKey(parent)] = append(m.composites[roleKey(parent)], children...)
}

// SetDefaultRoles sets the roles every person holds.
func (m *Memory) SetDefaultRoles(roles ...ClientRole) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultRoles = append([]ClientRole(nil), roles...)
}

// EffectiveClientRoles answers the names of clientID's roles the person
// holds in effect, the way Keycloak's role-mappings/clients/{client}/composite
// does: directly, through their Groups and the Groups above them, through
// the default roles, and through composites at any depth.
func (m *Memory) EffectiveClientRoles(_ context.Context, userID uuid.UUID, clientID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("EffectiveClientRoles")
	if _, ok := m.people[userID]; !ok {
		return nil, ErrNotFound
	}
	queue := append([]ClientRole(nil), m.userRoles[userID]...)
	queue = append(queue, m.defaultRoles...)
	for gid, members := range m.members {
		if _, ok := members[userID]; !ok {
			continue
		}
		g, ok := m.groups[gid]
		if !ok {
			continue
		}
		for id, other := range m.groups {
			if other.Path == g.Path || strings.HasPrefix(g.Path, other.Path+"/") {
				queue = append(queue, m.groupRoles[id]...)
			}
		}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]
		if _, ok := seen[roleKey(r)]; ok {
			continue
		}
		seen[roleKey(r)] = struct{}{}
		if r.ClientID == clientID {
			out = append(out, r.Role)
		}
		queue = append(queue, m.composites[roleKey(r)]...)
	}
	sort.Strings(out)
	return out, nil
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

// PutUser puts an enabled person.
func (m *Memory) PutUser(p Person) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	m.people[p.ID] = p
	delete(m.disabled, p.ID)
}

// answerLocked is p as the directory's reads answer it: enabled unless
// DisableUser disabled them. The caller holds m.mu.
func (m *Memory) answerLocked(p Person) Person {
	_, disabled := m.disabled[p.ID]
	p.Enabled = !disabled
	return p
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

// SetGroupParent makes parentID the id GroupNode answers above the Group
// id, whatever the paths say ("" for none): a Keycloak whose names hold a
// "/".
func (m *Memory) SetGroupParent(id, parentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parents[id] = parentID
}

// GroupNode answers the Group id, by its id alone, with the id of the Group
// above it: the one SetGroupParent set, or the Group at the path above.
func (m *Memory) GroupNode(_ context.Context, id string) (Group, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("GroupNode")
	g, ok := m.groups[id]
	if !ok {
		return Group{}, "", ErrNotFound
	}
	if parent, set := m.parents[id]; set {
		return g, parent, nil
	}
	cut := strings.LastIndex(g.Path, "/")
	if cut <= 0 {
		return g, "", nil
	}
	for _, other := range m.groups {
		if other.Path == g.Path[:cut] {
			return g, other.ID, nil
		}
	}
	return g, "", nil
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

// DeleteGroup mirrors Keycloak's recursive delete. The service refuses
// non-leaves before reaching this directory operation.
func (m *Memory) DeleteGroup(_ context.Context, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("DeleteGroup")
	g, ok := m.groups[groupID]
	if !ok {
		return ErrNotFound
	}
	for id, child := range m.groups {
		if id == groupID || strings.HasPrefix(child.Path, g.Path+"/") {
			delete(m.groups, id)
			delete(m.members, id)
			delete(m.groupRoles, id)
			delete(m.parents, id)
		}
	}
	return nil
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
			out = append(out, m.answerLocked(p))
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
		out = append(out, m.answerLocked(p))
	}
	return out, nil
}

func (m *Memory) SearchUsers(_ context.Context, query string, limit int) ([]Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("SearchUsers")
	// Keycloak reads *q* as an infix search; the memory directory always
	// searches infix.
	query = strings.Trim(strings.TrimSpace(query), "*")
	out := make([]Person, 0)
	for _, person := range m.people {
		if personMatches(person, query) {
			out = append(out, m.answerLocked(person))
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
	if have.ClientID != clientID && !(clientID == "forms" && have.ClientID == "skyforms") {
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
		out = append(out, m.answerLocked(p))
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

func (m *Memory) ClientRoleHolders(_ context.Context, clientID, role string) ([]Group, []Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("ClientRoleHolders")
	holds := func(roles []ClientRole) bool {
		return slices.ContainsFunc(roles, func(r ClientRole) bool { return holdsClientRole(r, clientID, role) })
	}
	groups, people := make([]Group, 0), make([]Person, 0)
	for gid, roles := range m.groupRoles {
		if g, ok := m.groups[gid]; ok && holds(roles) {
			groups = append(groups, g)
		}
	}
	for id, roles := range m.userRoles {
		if p, ok := m.people[id]; ok && holds(roles) {
			people = append(people, m.answerLocked(p))
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Path < groups[j].Path })
	sort.Slice(people, func(i, j int) bool { return people[i].ID.String() < people[j].ID.String() })
	return groups, people, nil
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
	return m.answerLocked(p), nil
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

func (m *Memory) DisableUser(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("DisableUser")
	if _, ok := m.people[id]; !ok {
		return ErrNotFound
	}
	m.disabled[id] = struct{}{}
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

// GroupProtection answers as Keycloak does (Keycloak.GroupProtection): a
// realm role (a ClientRole with no ClientID) or a client role that is no
// app role mapped to the Group, or held by a composite app role it maps at
// any depth.
func (m *Memory) GroupProtection(_ context.Context, groupID string) (Protection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("GroupProtection")
	g, ok := m.groups[groupID]
	if !ok {
		return Protection{}, ErrNotFound
	}
	seen := map[string]bool{}
	var check func(r ClientRole, depth int) Protection
	check = func(r ClientRole, depth int) Protection {
		switch {
		case r.ClientID == "":
			return Protection{Protected: true, Reason: "realm role " + r.Role}
		case seen[roleKey(r)]:
			return Protection{}
		}
		if got := clientRoleProtection(r.ClientID, r.Role); got.Protected {
			return got
		}
		seen[roleKey(r)] = true
		if depth >= maxCompositeDepth && len(m.composites[roleKey(r)]) > 0 {
			return Protection{Protected: true, Reason: "composite " + r.ClientID + ":" + r.Role + " nested too deep"}
		}
		for _, child := range m.composites[roleKey(r)] {
			if got := check(child, depth+1); got.Protected {
				got.Reason = "composite " + r.ClientID + ":" + r.Role + " holds " + got.Reason
				return got
			}
		}
		return Protection{}
	}
	for _, r := range m.groupRoles[g.ID] {
		if got := check(r, 0); got.Protected {
			return got, nil
		}
	}
	return Protection{}, nil
}

// KeycloakAdminRole is a realm-management role the Group groupID maps,
// directly or inside a composite, "" for none (KeycloakAdminGroups).
func (m *Memory) KeycloakAdminRole(_ context.Context, groupID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("KeycloakAdminRole")
	g, ok := m.groups[groupID]
	if !ok {
		return "", ErrNotFound
	}
	seen := map[string]bool{}
	var find func(r ClientRole) string
	find = func(r ClientRole) string {
		if r.ClientID == KeycloakAdminClient {
			return r.Role
		}
		if seen[roleKey(r)] {
			return ""
		}
		seen[roleKey(r)] = true
		for _, child := range m.composites[roleKey(r)] {
			if got := find(child); got != "" {
				return got
			}
		}
		return ""
	}
	for _, r := range m.groupRoles[g.ID] {
		if got := find(r); got != "" {
			return got, nil
		}
	}
	return "", nil
}

// RoleHoldsKeycloakAdmin is the realm-management role role is or holds
// as a composite, "" for none; a realm role inside counts as one
// (KeycloakAdminGroups).
func (m *Memory) RoleHoldsKeycloakAdmin(_ context.Context, role ClientRole) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("RoleHoldsKeycloakAdmin")
	seen := map[string]bool{}
	var find func(r ClientRole) string
	find = func(r ClientRole) string {
		switch {
		case r.ClientID == KeycloakAdminClient:
			return r.Role
		case seen[roleKey(r)]:
			return ""
		}
		seen[roleKey(r)] = true
		for _, child := range m.composites[roleKey(r)] {
			if child.ClientID == "" {
				return "realm role " + child.Role
			}
			if got := find(child); got != "" {
				return got
			}
		}
		return ""
	}
	return find(role), nil
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
