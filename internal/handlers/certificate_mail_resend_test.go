package handlers

import (
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

// queueCertMail takes queued certificate mails (certificate.CertificateMailer).
type queueCertMail struct{ recCertMail }

func (*queueCertMail) CertificateMail(context.Context, string, string, string, map[string]string) error {
	return nil
}

// POST /v1/events/{id}/certificates/mail/resend and
// POST /v1/certificates/{serial}/mail/resend (docs/certificate-mail.md): 403
// for who may not issue, 400 for an unknown scope, 202 with what was queued,
// 429 for a second resend to all within the cooldown.
func TestCertificateMailResendHTTP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	store := certificate.NewMemoryStore()
	certs := certificate.NewServiceWithOptions(store, tickets, events, user.NewMemoryStore(), az, staticPDF("%PDF-1.4"), &queueCertMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: store, Jobs: store,
	})
	ev, err := events.Create(ctx, event.Event{Name: "ARTLAB 2026", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleOnce})
	if err != nil {
		t.Fatal(err)
	}
	item, err := tickets.Create(ctx, ticket.Ticket{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ece", GuestLastName: "Su", GuestEmail: "ece@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := store.Create(ctx, certificate.Certificate{ID: uuid.New(), EventID: ev.ID, TicketID: item.ID, Serial: "SERIAL1", RecipientName: "Ece Su", RecipientEmail: "ece@example.com", EventName: ev.Name, OwnerTeam: "ARTLAB", VerifyURL: "https://skyl.app/c/SERIAL1"}, []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}

	leader := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	member := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	post := func(who authn.Identity, path, body string) (int, certificate.MailResend) {
		t.Helper()
		ch := NewCertificateHandler(certs)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, who)
			return c.Next()
		})
		app.Post("/v1/events/:eventId/certificates/mail/resend", ch.ResendEventMails)
		app.Post("/v1/certificates/:serial/mail/resend", ch.ResendMail)
		req := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		var out certificate.MailResend
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	eventPath := "/v1/events/" + ev.ID.String() + "/certificates/mail/resend"
	if status, _ := post(member, eventPath, `{"scope":"all"}`); status != fiber.StatusForbidden {
		t.Fatalf("member %d", status)
	}
	if status, _ := post(leader, eventPath, `{"scope":"everyone"}`); status != fiber.StatusBadRequest {
		t.Fatalf("unknown scope %d", status)
	}
	if status, got := post(leader, eventPath, `{"scope":"all"}`); status != fiber.StatusAccepted || got.Queued != 1 {
		t.Fatalf("all %d %+v", status, got)
	}
	if status, _ := post(leader, eventPath, `{"scope":"all"}`); status != fiber.StatusTooManyRequests {
		t.Fatalf("all again within the cooldown %d", status)
	}
	if status, got := post(leader, "/v1/certificates/"+cert.Serial+"/mail/resend", ``); status != fiber.StatusAccepted || got.Queued != 0 || got.AlreadyQueued != 1 {
		t.Fatalf("one %d %+v", status, got)
	}
	if status, _ := post(member, "/v1/certificates/"+cert.Serial+"/mail/resend", ``); status != fiber.StatusForbidden && status != fiber.StatusNotFound {
		t.Fatalf("member one %d", status)
	}
}
