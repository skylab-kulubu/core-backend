package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The attendee import over HTTP (docs/attendee-import.md): the preview and
// the import answer the rows; the mail is on unless the body turns it off;
// the limits and the operator rule answer their codes; the log line names
// nobody.
func TestAttendeeImportHTTP(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	events := event.NewMemoryStore()
	store := ticket.NewMemoryStore()
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	app := func(ident authn.Identity, logs *bytes.Buffer) *fiber.App {
		h := NewTicketHandler(ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy())))
		h.logger = log.New(logs, "", 0)
		a := fiber.New()
		a.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, ident)
			return c.Next()
		})
		a.Post("/v1/events/:eventId/attendee-imports/preview", h.PreviewImport)
		a.Post("/v1/events/:eventId/attendee-imports", h.ImportAttendees)
		return a
	}
	var logs bytes.Buffer
	lead := app(authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, &logs)
	member := app(authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/GAMELAB"}}, &bytes.Buffer{})
	call := func(a *fiber.App, path string, body any) (int, string) {
		t.Helper()
		raw, ok := body.(string)
		if !ok {
			b, _ := json.Marshal(body)
			raw = string(b)
		}
		req := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(got)
	}
	base := "/v1/events/" + ev.ID.String() + "/attendee-imports"
	list := "Ad,Soyad,E-posta\nAda,Lovelace,ada@example.com\nGrace,,grace@example.com\n"

	status, body := call(lead, base+"/preview", map[string]any{"text": list})
	if status != fiber.StatusOK || !strings.Contains(body, `"imported":false`) || !strings.Contains(body, `"problem":"missing_last_name"`) ||
		!strings.Contains(body, `"mail":"notifications_off"`) {
		t.Fatalf("preview %d %s", status, body)
	}
	if status, body := call(member, base+"/preview", map[string]any{"text": list}); status != fiber.StatusForbidden {
		t.Fatalf("another team's member %d %s", status, body)
	}
	status, body = call(lead, base, map[string]any{"text": list, "sendMail": false})
	if status != fiber.StatusOK || !strings.Contains(body, `"imported":true`) || !strings.Contains(body, `"mail":"off"`) {
		t.Fatalf("import %d %s", status, body)
	}
	if listed, _ := store.ListByEventStatus(ctx, ev.ID, nil); len(listed) != 1 {
		t.Fatalf("%d tickets", len(listed))
	}
	if line := logs.String(); !strings.Contains(line, `"attendees_imported"`) || strings.Contains(line, "@") || strings.Contains(line, "Ada") {
		t.Fatalf("log %q", line)
	}

	for _, tc := range []struct {
		body any
		code string
		want int
	}{
		{map[string]any{"text": ""}, "import_empty", fiber.StatusBadRequest},
		{map[string]any{"text": "Ad,Soyad\nA,B\n"}, "import_no_email_column", fiber.StatusBadRequest},
		{map[string]any{"text": "Ad Soyad,E-posta,Okul e-postası\nA B,a@example.com,b@example.com\n"}, `"columns":["E-posta","Okul e-postası"]`, fiber.StatusBadRequest},
		{map[string]any{"text": "ad,soyad,eposta\n" + strings.Repeat("a,b,c@example.com\n", ticket.MaxImportRows+1)}, "import_too_many_rows", fiber.StatusRequestEntityTooLarge},
		{map[string]any{"text": strings.Repeat("x", ticket.MaxImportBytes+1)}, "import_too_large", fiber.StatusRequestEntityTooLarge},
		{`{"text":`, "", fiber.StatusBadRequest},
	} {
		status, body := call(lead, base+"/preview", tc.body)
		if status != tc.want || !strings.Contains(body, tc.code) {
			t.Errorf("%s: %d %.200s", tc.code, status, body)
		}
	}
}

// With the Team audit record on (production), the preview and the import
// each open their row (ticket.import.preview, ticket.import) under the
// caller: both routes hand the service a ctx that carries them. A route
// without one would answer 503 audit_unavailable and write nothing.
func TestAttendeeImportHTTPRecordsTheCaller(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	audit := teamaudit.NewMemory()
	events := event.NewMemoryStore()
	store := ticket.NewMemoryStore()
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	lead := uuid.New()
	h := NewTicketHandler(ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()), dir, audit))
	h.logger = log.New(io.Discard, "", 0)
	a := fiber.New()
	a.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: lead, Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}})
		return c.Next()
	})
	a.Post("/v1/events/:eventId/attendee-imports/preview", h.PreviewImport)
	a.Post("/v1/events/:eventId/attendee-imports", h.ImportAttendees)
	base := "/v1/events/" + ev.ID.String() + "/attendee-imports"
	body := `{"text":"Ad,Soyad,E-posta\nAda,Lovelace,ada@example.com\n","sendMail":false}`
	for _, path := range []string{base + "/preview", base} {
		req := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, got)
		}
	}
	rows := audit.Rows()
	if len(rows) != 2 {
		t.Fatalf("%d audit rows", len(rows))
	}
	for i, want := range []teamaudit.Action{teamaudit.ActionTicketImportPreview, teamaudit.ActionTicketImport} {
		if r := rows[i]; r.Action != want || r.Actor.ID != lead || r.Outcome != teamaudit.OutcomeOK {
			t.Errorf("row %d: %s by %s, %s", i, r.Action, r.Actor.ID, r.Outcome)
		}
	}
}
