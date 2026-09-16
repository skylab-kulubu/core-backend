package identity

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

type Memory struct {
	mu      sync.Mutex
	groups  map[string]Group
	people  map[uuid.UUID]Person
	members map[string]map[uuid.UUID]struct{}
	Ops     []string
}

func NewMemory() *Memory {
	return &Memory{
		groups:  make(map[string]Group),
		people:  make(map[uuid.UUID]Person),
		members: make(map[string]map[uuid.UUID]struct{}),
	}
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
		if g.Path == idOrPath {
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
	for _, members := range m.members {
		delete(members, id)
	}
	return nil
}
