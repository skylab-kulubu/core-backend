package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
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

// POST /v1/events/{id}/certificates/rescan (certificate × session ticket 03;
// docs/certificate-rule.md): 403 for who may not issue, 409 while the
// attendance is open, 202 with the batch, then 200 with queued 0 and no
// batch.
func TestCertificateRescanHTTP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	store := certificate.NewMemoryStore()
	certs := certificate.NewServiceWithOptions(store, tickets, events, user.NewMemoryStore(), az, staticPDF("%PDF-1.4"), &recCertMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: store, Jobs: store,
	})
	ev, err := events.Create(ctx, event.Event{Name: "ARTLAB 2026", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleOnce})
	if err != nil {
		t.Fatal(err)
	}
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Açılış", SpeakerName: "Ada", SessionType: "PRESENTATION", CertificateCounted: true})
	if err != nil {
		t.Fatal(err)
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
	late, err := tickets.Create(ctx, ticket.Ticket{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ece", GuestLastName: "Su", GuestEmail: "ece@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	leader := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	member := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	post := func(who authn.Identity) (int, []byte) {
		t.Helper()
		ch := NewCertificateHandler(certs)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, who)
			return c.Next()
		})
		app.Post("/v1/events/:eventId/certificates/rescan", ch.Rescan)
		resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/certificates/rescan", nil))
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	if status, raw := post(member); status != fiber.StatusForbidden {
		t.Fatalf("member %d %s", status, raw)
	}
	if status, raw := post(leader); status != fiber.StatusConflict {
		t.Fatalf("open attendance %d %s", status, raw)
	}
	if _, err := certs.Finalize(ctx, authz.Principal{ID: leader.ID.String(), Groups: leader.Groups}, ev.ID); err != nil {
		t.Fatal(err)
	}
	// Checked in after the attendance closed.
	if _, err := tickets.AddCheckIn(ctx, ticket.CheckIn{TicketID: late.ID, EventDayID: day.ID, SessionID: sess.ID}); err != nil {
		t.Fatal(err)
	}
	status, raw := post(leader)
	var result struct {
		Queued int                `json:"queued"`
		Batch  *certificate.Batch `json:"batch"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || status != fiber.StatusAccepted || result.Queued != 1 || result.Batch == nil || result.Batch.Reason != "rescan" {
		t.Fatalf("rescan %d %s", status, raw)
	}
	status, raw = post(leader)
	result.Batch = nil
	if err := json.Unmarshal(raw, &result); err != nil || status != fiber.StatusOK || result.Queued != 0 || result.Batch != nil {
		t.Fatalf("second rescan %d %s", status, raw)
	}
}
