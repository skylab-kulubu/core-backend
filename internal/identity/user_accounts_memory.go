package identity

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// MemoryAccounts is an AccountDirectory in memory (tests and local runs):
// accounts PutAccount gave it, with their credentials, links, sessions,
// lock and events. The password it was given is kept only as "set".
type MemoryAccounts struct {
	mu          sync.Mutex
	users       map[uuid.UUID]AccountUser
	credentials map[uuid.UUID][]AccountCredential
	links       map[uuid.UUID][]LinkedIdentity
	sessions    map[uuid.UUID][]AccountSession
	lockouts    map[uuid.UUID]AccountLockout
	events      map[uuid.UUID][]LoginEvent
	// NoEvents makes LoginEvents answer ErrEventsUnavailable, as Keycloak
	// does without view-events.
	NoEvents bool
	// PasswordPolicy, when set, refuses a password shorter than it with
	// the realm's message key.
	PasswordPolicy int
	// FailMail makes the mails fail as an SMTP that is down does.
	FailMail bool
	// Passwords counts the temporary passwords set per person, Mails the
	// mails sent ("actions:UPDATE_PASSWORD", "verify"), Logouts the
	// sessions ended.
	Passwords map[uuid.UUID]int
	Mails     map[uuid.UUID][]string
	Logouts   map[uuid.UUID]int
}

// NewMemoryAccounts is an empty MemoryAccounts.
func NewMemoryAccounts() *MemoryAccounts {
	return &MemoryAccounts{
		users: map[uuid.UUID]AccountUser{}, credentials: map[uuid.UUID][]AccountCredential{},
		links: map[uuid.UUID][]LinkedIdentity{}, sessions: map[uuid.UUID][]AccountSession{},
		lockouts: map[uuid.UUID]AccountLockout{}, events: map[uuid.UUID][]LoginEvent{},
		Passwords: map[uuid.UUID]int{}, Mails: map[uuid.UUID][]string{}, Logouts: map[uuid.UUID]int{},
	}
}

// PutAccount stores the account as given.
func (m *MemoryAccounts) PutAccount(u AccountUser) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[u.ID] = cloneAccount(u)
}

// PutCredentials, PutLinks, PutSessions, PutLockout and PutEvents set the
// account's credentials, provider links, sessions, lock and events.
func (m *MemoryAccounts) PutCredentials(id uuid.UUID, c ...AccountCredential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.credentials[id] = slices.Clone(c)
}

func (m *MemoryAccounts) PutLinks(id uuid.UUID, l ...LinkedIdentity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.links[id] = slices.Clone(l)
}

func (m *MemoryAccounts) PutSessions(id uuid.UUID, s ...AccountSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = slices.Clone(s)
}

func (m *MemoryAccounts) PutLockout(id uuid.UUID, l AccountLockout) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockouts[id] = l
}

func (m *MemoryAccounts) PutEvents(id uuid.UUID, e ...LoginEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events[id] = slices.Clone(e)
}

// Get answers the account as stored now.
func (m *MemoryAccounts) Get(id uuid.UUID) (AccountUser, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	return cloneAccount(u), ok
}

func cloneAccount(u AccountUser) AccountUser {
	u.RequiredActions = slices.Clone(u.RequiredActions)
	attributes := make(map[string][]string, len(u.Attributes))
	for name, values := range u.Attributes {
		attributes[name] = slices.Clone(values)
	}
	u.Attributes = attributes
	return u
}

func (m *MemoryAccounts) user(id uuid.UUID) (AccountUser, error) {
	u, ok := m.users[id]
	if !ok {
		return AccountUser{}, ErrNotFound
	}
	return u, nil
}

func (m *MemoryAccounts) AccountUser(_ context.Context, id uuid.UUID) (AccountUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := m.user(id)
	return cloneAccount(u), err
}

func (m *MemoryAccounts) AccountCredentials(_ context.Context, id uuid.UUID) ([]AccountCredential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.user(id); err != nil {
		return nil, err
	}
	return slices.Clone(m.credentials[id]), nil
}

func (m *MemoryAccounts) LinkedIdentities(_ context.Context, id uuid.UUID) ([]LinkedIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.user(id); err != nil {
		return nil, err
	}
	return slices.Clone(m.links[id]), nil
}

func (m *MemoryAccounts) AccountSessions(_ context.Context, id uuid.UUID) ([]AccountSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.user(id); err != nil {
		return nil, err
	}
	return slices.Clone(m.sessions[id]), nil
}

func (m *MemoryAccounts) Lockout(_ context.Context, id uuid.UUID) (AccountLockout, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.user(id); err != nil {
		return AccountLockout{}, err
	}
	return m.lockouts[id], nil
}

func (m *MemoryAccounts) LoginEvents(_ context.Context, id uuid.UUID, limit int) ([]LoginEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NoEvents {
		return nil, ErrEventsUnavailable
	}
	events := slices.Clone(m.events[id])
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

func (m *MemoryAccounts) AddressHolders(_ context.Context, address string) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []uuid.UUID
	for id, u := range m.users {
		if strings.EqualFold(u.Email, address) || strings.EqualFold(u.attribute(schoolEmailAttribute), address) ||
			strings.EqualFold(u.attribute(personalEmailAttribute), address) {
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *MemoryAccounts) SetEnabled(_ context.Context, id uuid.UUID, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := m.user(id)
	if err != nil {
		return err
	}
	u.Enabled = enabled
	m.users[id] = u
	return nil
}

func (m *MemoryAccounts) SetTemporaryPassword(_ context.Context, id uuid.UUID, password string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := m.user(id)
	if err != nil {
		return err
	}
	if len(password) < m.PasswordPolicy {
		return &AccountError{Status: 422, Code: AccountPasswordPolicy, Policy: "invalidPasswordMinLengthMessage"}
	}
	if !slices.Contains(u.RequiredActions, "UPDATE_PASSWORD") {
		u.RequiredActions = append(u.RequiredActions, "UPDATE_PASSWORD")
	}
	m.users[id] = u
	m.Passwords[id]++
	return nil
}

func (m *MemoryAccounts) mail(id uuid.UUID, what string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := m.user(id)
	if err != nil {
		return err
	}
	if u.Email == "" {
		return &AccountError{Status: 409, Code: AccountNoEmail}
	}
	if m.FailMail {
		return &AccountError{Status: 502, Code: AccountMailFailed}
	}
	m.Mails[id] = append(m.Mails[id], what)
	return nil
}

func (m *MemoryAccounts) SendActionsMail(_ context.Context, id uuid.UUID, actions []string) error {
	return m.mail(id, "actions:"+strings.Join(actions, ","))
}

func (m *MemoryAccounts) SendVerificationMail(_ context.Context, id uuid.UUID) error {
	return m.mail(id, "verify")
}

func (m *MemoryAccounts) UpdateAccount(_ context.Context, id uuid.UUID, change AccountUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := m.user(id)
	if err != nil {
		return err
	}
	u = cloneAccount(u)
	if change.Email != nil {
		for other, holder := range m.users {
			if other != id && strings.EqualFold(holder.Email, *change.Email) {
				return &AccountError{Status: 409, Code: AccountEmailTaken}
			}
		}
		u.Email = *change.Email
	}
	if change.FirstName != nil {
		u.FirstName = *change.FirstName
	}
	if change.LastName != nil {
		u.LastName = *change.LastName
	}
	if change.EmailVerified != nil {
		u.EmailVerified = *change.EmailVerified
	}
	if change.RequiredActions != nil {
		u.RequiredActions = slices.Clone(*change.RequiredActions)
	}
	for name, value := range change.Attributes {
		if value == "" {
			delete(u.Attributes, name)
		} else {
			u.Attributes[name] = []string{value}
		}
	}
	m.users[id] = u
	return nil
}

func (m *MemoryAccounts) DeleteCredential(_ context.Context, id uuid.UUID, credentialID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := len(m.credentials[id])
	m.credentials[id] = slices.DeleteFunc(m.credentials[id], func(c AccountCredential) bool { return c.ID == credentialID })
	if len(m.credentials[id]) == before {
		return ErrNotFound
	}
	return nil
}

func (m *MemoryAccounts) UnlinkIdentity(_ context.Context, id uuid.UUID, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := len(m.links[id])
	m.links[id] = slices.DeleteFunc(m.links[id], func(l LinkedIdentity) bool { return l.Provider == provider })
	if len(m.links[id]) == before {
		return ErrNotFound
	}
	return nil
}

func (m *MemoryAccounts) ClearLockout(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.lockouts, id)
	return nil
}

func (m *MemoryAccounts) LogoutAllSessions(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	m.Logouts[id]++
	return nil
}
