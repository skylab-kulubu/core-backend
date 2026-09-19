package eventmail

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type memoryLists struct {
	mu      sync.Mutex
	creates int
	gone    map[uuid.UUID]bool
	names   map[uuid.UUID]string
	recs    map[uuid.UUID][]mail.ListRecipient
}

func newMemoryLists() *memoryLists {
	return &memoryLists{
		gone:  map[uuid.UUID]bool{},
		names: map[uuid.UUID]string{},
		recs:  map[uuid.UUID][]mail.ListRecipient{},
	}
}

func (m *memoryLists) CreateList(_ context.Context, name string) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creates++
	id := uuid.New()
	m.names[id] = name
	m.recs[id] = []mail.ListRecipient{}
	return id, nil
}

func (m *memoryLists) GetList(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gone[id] {
		return mail.ErrListNotFound
	}
	if _, ok := m.names[id]; !ok {
		return mail.ErrListNotFound
	}
	return nil
}

func (m *memoryLists) DeleteList(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.names[id]; !ok {
		return mail.ErrListNotFound
	}
	m.gone[id] = true
	delete(m.names, id)
	delete(m.recs, id)
	return nil
}

func (m *memoryLists) Recipients(_ context.Context, id uuid.UUID) ([]mail.ListRecipient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail.ListRecipient{}, m.recs[id]...), nil
}

func (m *memoryLists) AddRecipient(_ context.Context, id uuid.UUID, r mail.ListRecipient) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.recs[id] {
		if strings.EqualFold(existing.Email, r.Email) {
			return nil
		}
	}
	r.ID = uuid.New()
	m.recs[id] = append(m.recs[id], r)
	return nil
}

func (m *memoryLists) RemoveRecipient(_ context.Context, listID, recipientID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := make([]mail.ListRecipient, 0)
	for _, r := range m.recs[listID] {
		if r.ID != recipientID {
			kept = append(kept, r)
		}
	}
	m.recs[listID] = kept
	return nil
}

func mailLeader() authz.Principal {
	return authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
}

func TestSyncCreatesEventListOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	lists := newMemoryLists()
	ev, err := events.Create(ctx, event.Event{Name: "SkyDays", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, _, err := users.Upsert(ctx, user.User{ID: owner, Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Create(ctx, ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(events, tickets, users, lists, authz.NewAuthorizer(authz.DefaultPolicy()))
	first, err := svc.Sync(ctx, mailLeader(), ev.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.MailListID == uuid.Nil || first.RecipientCount != 2 || first.Name != "WEBLAB SkyDays" {
		t.Fatalf("first %+v", first)
	}
	second, err := svc.Sync(ctx, mailLeader(), ev.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.MailListID != first.MailListID {
		t.Fatalf("list %v vs %v", second.MailListID, first.MailListID)
	}
	if lists.creates != 1 {
		t.Fatalf("creates %d", lists.creates)
	}
	got, err := events.Get(ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MailListID == nil || *got.MailListID != first.MailListID {
		t.Fatalf("stored %+v", got.MailListID)
	}
}

func TestSyncRecreatesMissingList(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	lists := newMemoryLists()
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	svc := New(events, tickets, users, lists, authz.NewAuthorizer(authz.DefaultPolicy()))
	first, err := svc.Sync(ctx, mailLeader(), ev.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	lists.mu.Lock()
	lists.gone[first.MailListID] = true
	lists.mu.Unlock()
	second, err := svc.Sync(ctx, mailLeader(), ev.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.MailListID == first.MailListID {
		t.Fatal("expected a new list")
	}
	if lists.creates != 2 {
		t.Fatalf("creates %d", lists.creates)
	}
}

func TestSyncForbiddenForMember(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	events := event.NewMemoryStore()
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	svc := New(events, ticket.NewMemoryStore(), user.NewMemoryStore(), newMemoryLists(), authz.NewAuthorizer(authz.DefaultPolicy()))
	_, err = svc.Sync(ctx, authz.Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}}, ev.ID, nil)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err %v", err)
	}
}
