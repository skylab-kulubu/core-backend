package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// GET /v1/events/{id}/certificates/attendance and the manual issue's
// justification (certificate × session ticket 02; docs/certificate-rule.md).
func TestCertificateAttendanceHTTP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	store := certificate.NewMemoryStore()
	certs := certificate.NewServiceWithOptions(store, tickets, events, users, az, staticPDF("%PDF-1.4"), &recCertMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: store, Jobs: store,
	})
	ev, err := events.Create(ctx, event.Event{Name: "ARTLAB 2026", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleCount})
	if err != nil {
		t.Fatal(err)
	}
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := []event.Session{}
	for i, title := range []string{"Açılış", "Talk 1", "Talk 2", "Talk 3"} {
		sess, err := events.CreateSession(ctx, event.Session{
			EventDayID: day.ID, Title: title, SpeakerName: "Ada", SessionType: "PRESENTATION", OrderIndex: i,
			CertificateCounted: true, CertificateRequired: title == "Açılış",
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, sess)
	}
	template, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "sky", DraftLayout: certificate.Layout{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeOwnerTeam, ScopeKey: "ARTLAB", TemplateID: template.ID}); err != nil {
		t.Fatal(err)
	}
	ownerID := uuid.New()
	if _, _, err := users.Upsert(ctx, user.User{ID: ownerID, Email: "ayse@example.com", FirstName: "Ayşe", LastName: "Yılmaz"}); err != nil {
		t.Fatal(err)
	}
	below, err := tickets.Create(ctx, ticket.Ticket{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &ownerID})
	if err != nil {
		t.Fatal(err)
	}
	// Two counted check-ins of four (threshold 3), Açılış among them.
	for _, sess := range sessions[:2] {
		if _, err := tickets.AddCheckIn(ctx, ticket.CheckIn{TicketID: below.ID, EventDayID: day.ID, SessionID: sess.ID}); err != nil {
			t.Fatal(err)
		}
	}

	leader := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	member := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	appAs := func(who authn.Identity) *fiber.App {
		ch := NewCertificateHandler(certs)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, who)
			return c.Next()
		})
		app.Get("/v1/events/:eventId/certificates/attendance", ch.Attendance)
		app.Post("/v1/events/:eventId/certificates/issue", ch.Issue)
		return app
	}
	send := func(app *fiber.App, method, path, body string) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = bytes.NewReader([]byte(body))
		}
		req := httptest.NewRequest(method, path, reader)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	path := "/v1/events/" + ev.ID.String() + "/certificates/attendance"

	status, raw := send(appAs(leader), fiber.MethodGet, path, "")
	if status != fiber.StatusOK {
		t.Fatalf("leader list %d %s", status, raw)
	}
	var list certificate.AttendanceList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if list.CountedTotal != 4 || list.EffectiveMin != 3 || len(list.Rows) != 1 || list.Rows[0].Name != "Ayşe Yılmaz" ||
		list.Rows[0].Attended != 2 || list.Rows[0].Status != certificate.StatusBelow || len(list.Rows[0].MissingRequired) != 0 {
		t.Fatalf("list %s", raw)
	}
	if strings.Contains(string(raw), "ayse@example.com") {
		t.Fatalf("the list carries the e-mail: %s", raw)
	}
	if status, raw := send(appAs(member), fiber.MethodGet, path, ""); status != fiber.StatusForbidden {
		t.Fatalf("member list %d %s", status, raw)
	}

	issue := "/v1/events/" + ev.ID.String() + "/certificates/issue"
	code := func(raw []byte) string {
		var problem map[string]any
		_ = json.Unmarshal(raw, &problem)
		got, _ := problem["code"].(string)
		return got
	}
	status, raw = send(appAs(leader), fiber.MethodPost, issue, `{"ticketId":"`+below.ID.String()+`"}`)
	if status != fiber.StatusBadRequest || code(raw) != "justification_required" {
		t.Fatalf("below without a justification %d %s", status, raw)
	}
	status, raw = send(appAs(leader), fiber.MethodPost, issue, `{"ticketId":"`+below.ID.String()+`","justification":"`+strings.Repeat("a", 501)+`"}`)
	if status != fiber.StatusBadRequest || code(raw) != "justification_too_long" {
		t.Fatalf("501 characters %d %s", status, raw)
	}
	status, raw = send(appAs(leader), fiber.MethodPost, issue, `{"ticketId":"`+below.ID.String()+`","justification":"Raporlu, iki oturumu kaçırdı"}`)
	if status != fiber.StatusAccepted {
		t.Fatalf("below with a justification %d %s", status, raw)
	}
	var batch certificate.Batch
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Justification == nil || *batch.Justification != "Raporlu, iki oturumu kaçırdı" || batch.Reason != "manual" {
		t.Fatalf("batch %s", raw)
	}
	status, raw = send(appAs(leader), fiber.MethodGet, path, "")
	if status != fiber.StatusOK || !strings.Contains(string(raw), `"status":"queued"`) {
		t.Fatalf("after the manual issue %d %s", status, raw)
	}
}
