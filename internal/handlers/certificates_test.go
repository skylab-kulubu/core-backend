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

type staticPDF []byte

func (s staticPDF) PDF(context.Context, string) ([]byte, error) {
	return []byte(s), nil
}

type recCertMail struct{ n int }

func (r *recCertMail) Certificate(context.Context, string, string, map[string]string) {
	r.n++
}

func TestCertificateHTTP_VerifyPDFAndCheckInIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	mailer := &recCertMail{}
	certs := certificate.NewService(certificate.NewMemoryStore(), tickets, events, users, az, staticPDF("%PDF-1.4"), mailer, "https://api.example.test")
	ticketSvc := ticket.WithSettledCheckIn(ticket.NewService(tickets, events, az, users), func(ctx context.Context, ticketID uuid.UUID) {
		_, _ = certs.RecomputeTicket(ctx, ticketID)
	})

	ratio := 0.75
	ev, err := events.Create(ctx, event.Event{
		Name: "ARTLAB 2026", Location: "YTÜ", OwnerTeam: "ARTLAB",
		AttendanceRule: certificate.RuleRatio, AttendanceRatio: &ratio,
	})
	if err != nil {
		t.Fatal(err)
	}
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := make([]event.Session, 8)
	for i := 0; i < 8; i++ {
		sess, err := events.CreateSession(ctx, event.Session{
			EventDayID: day.ID, Title: "Talk", SpeakerName: "Ada", SessionType: "PRESENTATION",
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions[i] = sess
	}
	ownerID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, _, err := users.Upsert(ctx, user.User{ID: ownerID, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	tk, err := ticketSvc.Apply(ctx, authz.Principal{ID: ownerID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leadP := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	for i := 0; i < 5; i++ {
		if _, err := ticketSvc.CheckIn(ctx, leadP, tk.ID, sessions[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	if mailer.n != 0 {
		t.Fatalf("mail before eligible %d", mailer.n)
	}
	if _, err := ticketSvc.CheckIn(ctx, leadP, tk.ID, sessions[5].ID); err != nil {
		t.Fatal(err)
	}
	if mailer.n != 1 {
		t.Fatalf("mail after sixth %d", mailer.n)
	}
	listed, err := certs.ListByEvent(ctx, leadP, ev.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed %v %+v", err, listed)
	}
	serial := listed[0].Serial

	ch := NewCertificateHandler(certs)
	app := fiber.New()
	app.Get("/v1/certificates/verify/:serial", ch.Verify)
	app.Get("/v1/certificates/verify/:serial/pdf", ch.Download)
	app.Get("/v1/certificates/verify/:serial/qr", ch.QR)
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: ownerID, Groups: []string{"/UYELER/ARGE/ARTLAB"}})
		return c.Next()
	})
	app.Post("/v1/events/:eventId/certificates/issue", ch.Issue)
	app.Post("/v1/certificates/:serial/revoke", ch.Revoke)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/certificates/verify/"+serial, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("verify %d %s", resp.StatusCode, body)
	}
	var got certificate.Certificate
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Serial != serial || got.OwnerTeam != "ARTLAB" {
		t.Fatalf("got %+v", got)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/certificates/verify/"+serial+"/pdf", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("pdf %d", resp.StatusCode)
	}
	pdf, _ := io.ReadAll(resp.Body)
	if string(pdf) != "%PDF-1.4" {
		t.Fatalf("pdf %q", pdf)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/certificates/verify/"+serial+"/qr", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("qr %d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/png") {
		t.Fatalf("qr type %s", resp.Header.Get("Content-Type"))
	}
	png, _ := io.ReadAll(resp.Body)
	if len(png) < 100 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("not png")
	}

	req := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/certificates/issue", bytes.NewReader([]byte(`{"ticketId":"`+tk.ID.String()+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("member issue %d %s", resp.StatusCode, body)
	}

	rev := httptest.NewRequest(fiber.MethodPost, "/v1/certificates/"+serial+"/revoke", nil)
	resp, err = app.Test(rev)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("member revoke %d %s", resp.StatusCode, body)
	}
}

func TestEventHTTP_AttendanceRule(t *testing.T) {
	t.Parallel()
	ident := weblabLeader()
	store := event.NewMemoryStore()
	app := eventApp(t, ident, store)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"ARTLAB","location":"YTÜ","ownerTeam":"WEBLAB","attendanceRule":"ratio","attendanceRatio":0.75}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var created event.Event
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.AttendanceRule != "ratio" || created.AttendanceRatio == nil || *created.AttendanceRatio != 0.75 {
		t.Fatalf("created %+v", created)
	}
	req = httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"X","location":"YTÜ","ownerTeam":"WEBLAB","attendanceRule":"ratio"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("missing ratio %d", resp.StatusCode)
	}
}
