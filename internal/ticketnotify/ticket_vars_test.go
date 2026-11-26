package ticketnotify_test

import (
	"encoding/json"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketnotify"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	memberKey = "MEMBERKEYABCDEFGHIJKLMNOPQ"
	guestKey  = "GUESTKEYABCDEFGHIJKLMNOPQR"
)

func withKeys(e *env, id uuid.UUID, key, code string) {
	t := e.tickets[id]
	t.AccessKey, t.Code = key, code
	e.tickets[id] = t
}

func pageWorker(t *testing.T, e *env, sky ticketnotify.Mailer, pageURL string) *ticketnotify.Service {
	t.Helper()
	worker, err := ticketnotify.NewService(ticketnotify.Config{
		Queue: e.queue, Tickets: e.tickets, Events: e.events, Users: e.users, Mail: sky,
		Page:      ticket.PageConfig{URL: pageURL, Grace: ticket.DefaultPageGrace},
		APIOrigin: "https://api.yildizskylab.com/",
		Now:       func() time.Time { return e.now },
		Logger:    log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

// TICKET_PAGE_URL unset: the five variables of before, member and guest.
// Set: the Ticket's page, QR image and code join them, for both alike.
func TestTicketVariablesFollowTheTicketPage(t *testing.T) {
	for _, pageURL := range []string{"", "https://yildizskylab.com/bilet/{key}"} {
		e := newEnv(t)
		m := e.member(user.AccountActive)
		withKeys(e, m.ID, memberKey, "K7M2-QX9F")
		g := e.guest("can@example.com")
		withKeys(e, g.ID, guestKey, "AB12-CD34")
		worker := pageWorker(t, e, e.mail, pageURL)
		if _, err := worker.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(e.mail.calls) != 2 {
			t.Fatalf("%d calls", len(e.mail.calls))
		}
		base := map[string]string{"EventId": e.ev.ID.String(), "EventName": "Kodlama Gecesi", "Venue": "D-Blok", "OwnerTeam": "WEBLAB", "EventDate": "10.11.2026 18:30", "CoOrganizers": "", "TicketNote": "",
			"Theme": "", "EventBannerUrl": ""}
		for _, c := range e.mail.calls {
			want := maps.Clone(base)
			if pageURL != "" {
				key, code := memberKey, "K7M2-QX9F"
				if c.holder.Email != "" {
					key, code = guestKey, "AB12-CD34"
				}
				want["TicketUrl"] = "https://yildizskylab.com/bilet/" + key
				want["TicketQrUrl"] = "https://api.yildizskylab.com/v1/public/tickets/" + key + "/qr.png"
				want["TicketCode"] = code
			}
			if !maps.Equal(c.vars, want) {
				t.Fatalf("page %q, holder %+v: vars %v, want %v", pageURL, c.holder, c.vars, want)
			}
			if _, ok := c.vars["EventUrl"]; ok {
				t.Fatal("EventUrl sent")
			}
		}
	}
}

// The bodies SkyNotify gets, as they go: a member's Notification by subject,
// a guest's mail by address, each with the eight variables, under the
// Ticket's key of before (:v1).
func TestTicketCreatedBodiesSnapshot(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]map[string]any{}
	keys := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		bodies[r.URL.Path] = body
		keys[r.URL.Path] = r.Header.Get("Idempotency-Key")
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"0b6f2a3e-8d1c-4f5e-9a7b-3c2d1e0f9a8b"}`))
	}))
	t.Cleanup(srv.Close)
	e := newEnv(t)
	m := e.member(user.AccountActive)
	withKeys(e, m.ID, memberKey, "K7M2-QX9F")
	g := e.guest("can@example.com")
	withKeys(e, g.ID, guestKey, "AB12-CD34")
	sky := &mail.SkyNotify{BaseURL: srv.URL, Tokens: mail.StaticToken("tok"), HTTP: srv.Client()}
	worker := pageWorker(t, e, sky, "https://yildizskylab.com/bilet/{key}")
	report, err := worker.Pass(t.Context())
	if err != nil || report.Sent != 2 {
		t.Fatalf("report %+v %v", report, err)
	}

	member := bodies["/v1/notifications"]
	got, _ := json.Marshal(map[string]any{"type": member["type"], "recipients": member["recipients"], "variables": member["variables"], "source": member["source"]})
	want, _ := json.Marshal(map[string]any{
		"type":       "event.ticket-created",
		"recipients": []any{map[string]any{"sub": m.OwnerID.String()}},
		"variables": map[string]any{
			"EventId": e.ev.ID.String(), "EventName": "Kodlama Gecesi", "EventDate": "10.11.2026 18:30", "Venue": "D-Blok", "OwnerTeam": "WEBLAB", "CoOrganizers": "",
			"TicketNote": "", "TicketNoteParts": []any{},
			"Theme": "", "EventBannerUrl": "",
			"TicketUrl":   "https://yildizskylab.com/bilet/" + memberKey,
			"TicketQrUrl": "https://api.yildizskylab.com/v1/public/tickets/" + memberKey + "/qr.png",
			"TicketCode":  "K7M2-QX9F",
		},
		"source": map[string]any{"kind": "ticket", "ref": m.ID.String()},
	})
	if string(got) != string(want) {
		t.Fatalf("member body\n got %s\nwant %s", got, want)
	}

	guest := bodies["/v1/mail_tasks/single"]
	gotVars, _ := json.Marshal(guest["body_variables"])
	wantVars, _ := json.Marshal(map[string]any{
		"EventId": e.ev.ID.String(), "EventName": "Kodlama Gecesi", "EventDate": "10.11.2026 18:30", "Venue": "D-Blok", "OwnerTeam": "WEBLAB", "CoOrganizers": "",
		"TicketNote": "", "TicketNoteParts": []any{},
		"Theme": "", "EventBannerUrl": "",
		"TicketUrl":   "https://yildizskylab.com/bilet/" + guestKey,
		"TicketQrUrl": "https://api.yildizskylab.com/v1/public/tickets/" + guestKey + "/qr.png",
		"TicketCode":  "AB12-CD34",
	})
	if string(gotVars) != string(wantVars) || guest["template_key"] != "event.ticket-created" || guest["recipient_email"] != "can@example.com" {
		t.Fatalf("guest body %v", guest)
	}
	if keys["/v1/notifications"] != "core:event.ticket-created:"+m.ID.String()+":v1" {
		t.Fatalf("member key %q", keys["/v1/notifications"])
	}
}
