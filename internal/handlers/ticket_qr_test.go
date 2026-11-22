package handlers

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type ticketQRDoor struct {
	t        *testing.T
	events   *event.MemoryStore
	tickets  *ticket.MemoryStore
	svc      ticket.Service
	signer   *ticketqr.Signer
	pass     skypass.Service
	ev       event.Event
	sess     event.Session
	second   event.Session
	staff    authn.Identity
	guest    ticket.Ticket
	member   ticket.Ticket
	memberID uuid.UUID
	scans    *ticket.DoorScanMetrics
}

func newTicketQRDoor(t *testing.T) *ticketQRDoor {
	t.Helper()
	users := user.NewMemoryStore()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	passKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	signer := ticketqr.NewSigner(ticketqr.DeriveKey(passKey), ticketqr.Config{})
	scans := &ticket.DoorScanMetrics{}
	svc := ticket.NewService(tickets, events, az, users, signer, scans)
	staffID := uuid.New()
	memberID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(t.Context(), memberID, user.Profile{Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper"}); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(48 * time.Hour)
	ev, err := events.Create(t.Context(), event.Event{Name: "Yapay Zeka Kampı", Location: "YTÜ", OwnerTeam: "AILAB", EndDate: &end, DoorStaffIDs: []uuid.UUID{staffID}})
	if err != nil {
		t.Fatal(err)
	}
	day, err := events.CreateDay(t.Context(), event.Day{EventID: ev.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := events.CreateSession(t.Context(), event.Session{EventDayID: day.ID, Title: "Açılış", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := events.CreateSession(t.Context(), event.Session{EventDayID: day.ID, Title: "Kapanış", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	lead := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/AILAB/LIDERLER"}}
	applied, err := svc.ApplyGuest(t.Context(), lead, ev.ID, ticket.GuestInfo{FirstName: "Elif", LastName: "Yılmaz", Email: "elif@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := svc.Apply(t.Context(), authz.Principal{ID: memberID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &ticketQRDoor{
		t: t, events: events, tickets: tickets, svc: svc, signer: signer, pass: skypass.NewService(users, az, skypass.NewSigner(passKey, time.Minute)),
		ev: ev, sess: sess, second: second, staff: authn.Identity{ID: staffID, Groups: []string{"/UYELER/ARGE/AILAB"}},
		guest: applied.Ticket, member: member, memberID: memberID, scans: scans,
	}
}

func (d *ticketQRDoor) app(ident authn.Identity) *fiber.App {
	h := NewSkyPassHandler(d.pass, d.svc).WithTicketQRKeys(d.signer)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/v1/skypass/jwks", h.JWKS)
	app.Get("/v1/door/jwks", h.JWKS)
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	})
	app.Post("/v1/sessions/:sessionId/check-in/skypass", h.CheckInSession)
	app.Post("/v1/sessions/:sessionId/check-in/scan", h.CheckInSession)
	return app
}

type scanAnswer struct {
	status int
	code   string
	detail string
	body   string
}

func (d *ticketQRDoor) scan(app *fiber.App, route string, sessionID uuid.UUID, token string) scanAnswer {
	d.t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/v1/sessions/"+sessionID.String()+"/check-in/"+route, strings.NewReader(`{"token":`+jsonString(token)+`}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		d.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &problem)
	return scanAnswer{status: resp.StatusCode, code: problem.Code, detail: problem.Detail, body: string(raw)}
}

func (d *ticketQRDoor) qr(t ticket.Ticket, ev event.Event) string {
	d.t.Helper()
	got, err := d.tickets.Get(d.t.Context(), t.ID)
	if err != nil {
		d.t.Fatal(err)
	}
	token, err := d.svc.TicketQR(got, ev)
	if err != nil {
		d.t.Fatal(err)
	}
	return token
}

// A Ticket QR at the door: a guest's and a member's code check in once per
// Session on either route; the second scan is 409.
func TestTicketQRChecksInAtTheDoor(t *testing.T) {
	t.Parallel()
	d := newTicketQRDoor(t)
	app := d.app(d.staff)
	for _, tc := range []struct {
		name    string
		route   string
		session event.Session
		ticket  ticket.Ticket
	}{
		{"guest, old route", "skypass", d.sess, d.guest},
		{"guest, new route", "scan", d.second, d.guest},
		{"member, new route", "scan", d.sess, d.member},
	} {
		got := d.scan(app, tc.route, tc.session.ID, d.qr(tc.ticket, d.ev))
		if got.status != fiber.StatusCreated {
			t.Fatalf("%s: %+v", tc.name, got)
		}
		var ci ticket.CheckIn
		if err := json.Unmarshal([]byte(got.body), &ci); err != nil || ci.ID == uuid.Nil || ci.TicketID != tc.ticket.ID || ci.SessionID != tc.session.ID {
			t.Fatalf("%s: %s", tc.name, got.body)
		}
		if again := d.scan(app, tc.route, tc.session.ID, d.qr(tc.ticket, d.ev)); again.status != fiber.StatusConflict {
			t.Fatalf("%s again: %+v", tc.name, again)
		}
	}
	// The member's SkyPass after their Ticket QR: one check-in, 409.
	pass, err := d.pass.Mint(t.Context(), authz.Principal{ID: d.memberID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.scan(app, "skypass", d.sess.ID, pass.Value); got.status != fiber.StatusConflict {
		t.Fatalf("SkyPass after the Ticket QR: %+v", got)
	}
	if text := d.scans.Prometheus(); !strings.Contains(text, `skylab_door_scans_total{credential="ticket_qr",outcome="checked_in"} 3`) ||
		!strings.Contains(text, `skylab_door_scans_total{credential="ticket_qr",outcome="already"} 3`) {
		t.Fatalf("metrics\n%s", text)
	}
}

// What the door says to a Ticket QR it may not take.
func TestTicketQRRefusals(t *testing.T) {
	t.Parallel()
	d := newTicketQRDoor(t)
	app := d.app(d.staff)
	good := d.qr(d.guest, d.ev)

	// Expired: a day after the Event ends, by its dates now.
	past := time.Now().Add(-26 * time.Hour)
	ended := d.ev
	ended.EndDate = &past
	pastEvent, err := d.events.Update(t.Context(), ended)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.scan(app, "scan", d.sess.ID, good); got.status != fiber.StatusUnauthorized || got.code != "ticket_qr_expired" {
		t.Fatalf("expired: %+v", got)
	}
	// Moved later: the code sent with the earlier dates works again.
	later := time.Now().Add(72 * time.Hour)
	pastEvent.EndDate = &later
	if _, err := d.events.Update(t.Context(), pastEvent); err != nil {
		t.Fatal(err)
	}
	stale := d.qr(d.guest, ended)
	if got := d.scan(app, "scan", d.second.ID, stale); got.status != fiber.StatusCreated {
		t.Fatalf("code of the earlier dates after the Event moved later: %+v", got)
	}

	// Another key: 401. The SkyPass key itself with kid tk-e1: 401.
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	foreign, _ := ticketqr.NewSigner(ticketqr.DeriveKey(otherKey), ticketqr.Config{}).Mint(ticketqr.Claims{
		Ticket: d.guest.ID, Event: d.ev.ID, Version: 1, ExpiresAt: time.Now().Add(time.Hour),
	})
	if got := d.scan(app, "scan", d.sess.ID, foreign); got.status != fiber.StatusUnauthorized || got.code != "ticket_qr_invalid" {
		t.Fatalf("another key: %+v", got)
	}

	// Another Event's Ticket: 404.
	other, err := d.events.Create(t.Context(), event.Event{Name: "Başka", Location: "YTÜ", OwnerTeam: "AILAB"})
	if err != nil {
		t.Fatal(err)
	}
	lead := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/AILAB/LIDERLER"}}
	elsewhere, err := d.svc.ApplyGuest(t.Context(), lead, other.ID, ticket.GuestInfo{FirstName: "Ali", LastName: "Veli", Email: "ali@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.scan(app, "scan", d.sess.ID, d.qr(elsewhere.Ticket, other)); got.status != fiber.StatusNotFound {
		t.Fatalf("another Event's Ticket: %+v", got)
	}
	// A code naming this Event but no Ticket of it: 404.
	ghost, _ := d.signer.Mint(ticketqr.Claims{Ticket: uuid.New(), Event: d.ev.ID, Version: 1, ExpiresAt: time.Now().Add(time.Hour)})
	if got := d.scan(app, "scan", d.sess.ID, ghost); got.status != fiber.StatusNotFound {
		t.Fatalf("no such Ticket: %+v", got)
	}

	// A QR version the Ticket does not have (renewing the link, ticket 05,
	// raises it): 410 with a Turkish detail.
	otherVersion, _ := d.signer.Mint(ticketqr.Claims{Ticket: d.member.ID, Event: d.ev.ID, Version: 2, ExpiresAt: time.Now().Add(time.Hour)})
	if got := d.scan(app, "scan", d.sess.ID, otherVersion); got.status != fiber.StatusGone || got.code != "ticket_qr_superseded" || !strings.Contains(got.detail, "yenilenmiş") {
		t.Fatalf("another version: %+v", got)
	}

	// A cancelled Event: 403 session_closed, before the code is read.
	if _, err := d.events.Cancel(t.Context(), d.ev.ID, nil, "", false); err != nil {
		t.Fatal(err)
	}
	if got := d.scan(app, "scan", d.sess.ID, d.qr(d.member, d.ev)); got.status != fiber.StatusForbidden || got.code != "session_closed" {
		t.Fatalf("cancelled: %+v", got)
	}
}

// A scanner who may not take check-ins here is refused (403) whatever the
// code, a valid one included: the door's rule comes before the code.
func TestTicketQRNeedsTheDoor(t *testing.T) {
	t.Parallel()
	d := newTicketQRDoor(t)
	member := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app := d.app(member)
	for name, token := range map[string]string{"valid": d.qr(d.guest, d.ev), "garbage": "eyJhbGciOiJFUzI1NiIsImtpZCI6InRrLWUxIn0.e30.AA"} {
		if got := d.scan(app, "scan", d.sess.ID, token); got.status != fiber.StatusForbidden {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	if got := d.scan(app, "scan", uuid.New(), d.qr(d.guest, d.ev)); got.status != fiber.StatusNotFound {
		t.Fatalf("no such Session: %+v", got)
	}
}

// The JWKS, under both names, holds SkyPass's key and the Ticket QR's.
func TestJWKSHoldsBothKeys(t *testing.T) {
	t.Parallel()
	d := newTicketQRDoor(t)
	app := d.app(authn.Identity{})
	for _, path := range []string{"/v1/skypass/jwks", "/v1/door/jwks"} {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		var set skypass.JWKS
		if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
			t.Fatal(err)
		}
		kids := []string{}
		for _, k := range set.Keys {
			kids = append(kids, k.Kid)
		}
		if strings.Join(kids, ",") != "sp-e1,tk-e1" {
			t.Fatalf("%s: kids %v", path, kids)
		}
	}
}
