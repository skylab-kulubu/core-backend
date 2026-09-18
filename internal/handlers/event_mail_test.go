package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventmail"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type handlerLists struct {
	mu      sync.Mutex
	creates int
	names   map[uuid.UUID]string
	recs    map[uuid.UUID][]mail.ListRecipient
}

func newHandlerLists() *handlerLists {
	return &handlerLists{names: map[uuid.UUID]string{}, recs: map[uuid.UUID][]mail.ListRecipient{}}
}

func (m *handlerLists) CreateList(_ context.Context, name string) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creates++
	id := uuid.New()
	m.names[id] = name
	m.recs[id] = nil
	return id, nil
}

func (m *handlerLists) GetList(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.names[id]; !ok {
		return mail.ErrListNotFound
	}
	return nil
}

func (m *handlerLists) Recipients(_ context.Context, id uuid.UUID) ([]mail.ListRecipient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail.ListRecipient{}, m.recs[id]...), nil
}

func (m *handlerLists) AddRecipient(_ context.Context, id uuid.UUID, r mail.ListRecipient) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.ID = uuid.New()
	m.recs[id] = append(m.recs[id], r)
	return nil
}

func (m *handlerLists) RemoveRecipient(_ context.Context, listID, recipientID uuid.UUID) error {
	return nil
}

func mailApp(t *testing.T, ident authn.Identity, events event.Store, tickets ticket.Store, users user.Store, lists mail.Lists) *fiber.App {
	t.Helper()
	h := NewEventMailHandler(eventmail.New(events, tickets, users, lists, authz.NewAuthorizer(authz.DefaultPolicy())))
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Post("/v1/events/:eventId/mail-list", h.Sync)
	return app
}

func TestEventMailListCreatesOnceForApplicants(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	lists := newHandlerLists()
	ev, err := events.Create(t.Context(), event.Event{Name: "SkyDays", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Create(t.Context(), ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestEmail: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	ident := weblabLeader()
	app := mailApp(t, ident, events, tickets, users, lists)
	path := "/v1/events/" + ev.ID.String() + "/mail-list"
	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var first eventmail.Result
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.MailListID == uuid.Nil || first.RecipientCount != 1 || first.Name != "WEBLAB SkyDays" {
		t.Fatalf("first %+v", first)
	}
	resp, err = app.Test(httptest.NewRequest(fiber.MethodPost, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var second eventmail.Result
	if err := json.NewDecoder(resp.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.MailListID != first.MailListID {
		t.Fatalf("list %v vs %v", second.MailListID, first.MailListID)
	}
	if lists.creates != 1 {
		t.Fatalf("creates %d", lists.creates)
	}
}

func TestEventMailListForbiddenForMember(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	member := authn.Identity{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app := mailApp(t, member, events, ticket.NewMemoryStore(), user.NewMemoryStore(), newHandlerLists())
	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/mail-list", strings.NewReader("")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestEventMailListSkymailForbiddenIs403(t *testing.T) {
	t.Parallel()
	sky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mailing_lists" || r.Method != http.MethodPost {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(sky.Close)

	events := event.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "SkyDays", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	lists := &mail.SkyMail{BaseURL: sky.URL, Tokens: mail.StaticToken("tok"), HTTP: sky.Client()}
	app := mailApp(t, weblabLeader(), events, ticket.NewMemoryStore(), user.NewMemoryStore(), lists)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/mail-list", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type %q", ct)
	}
	var problem map[string]any
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatal(err)
	}
	if problem["status"] != float64(fiber.StatusForbidden) {
		t.Fatalf("problem %+v", problem)
	}
	detail, _ := problem["detail"].(string)
	if !strings.Contains(detail, "skymail:lists:write") {
		t.Fatalf("detail %q", detail)
	}
}

func TestEventMailListSkymailOtherStatusIs502(t *testing.T) {
	t.Parallel()
	sky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(sky.Close)

	events := event.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "SkyDays", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	lists := &mail.SkyMail{BaseURL: sky.URL, Tokens: mail.StaticToken("tok"), HTTP: sky.Client()}
	app := mailApp(t, weblabLeader(), events, ticket.NewMemoryStore(), user.NewMemoryStore(), lists)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/mail-list", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusBadGateway {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var problem map[string]any
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatal(err)
	}
	detail, _ := problem["detail"].(string)
	if !strings.Contains(detail, "status 503") {
		t.Fatalf("detail %q", detail)
	}
}
