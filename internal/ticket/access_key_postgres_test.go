package ticket_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// storedKeys reads a Ticket's access key, code and QR version as the
// database holds them.
func (f formResponsePostgres) storedKeys(t *testing.T, id uuid.UUID) (string, string, int) {
	t.Helper()
	var key, code string
	var version int
	if err := f.pool.QueryRow(context.Background(), `SELECT access_key, code, qr_version FROM tickets WHERE id = $1`, id).Scan(&key, &code, &version); err != nil {
		t.Fatal(err)
	}
	return key, code, version
}

// Every path that writes a Ticket writes it with its access key, its code
// and QR version 1: Member apply, Apply-for-other, Guest apply, a Forms
// answer (signed in and guest) and a guest the panel adds to a draft (K5).
func TestEveryTicketWrittenHasAnAccessKeyAndCode(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	store := ticket.NewPostgresStore(f.pool)
	svc := f.service(store)
	ev, err := f.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(48 * time.Hour)
	draft, err := f.events.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	formID := uuid.New()
	formEvent := seedFormEvent(t, f.events, formAddress(formID))

	member := f.person(t, "member@example.com")
	other := f.person(t, "other@example.com")
	answering := f.person(t, "answering@example.com")

	written := map[string]func() (ticket.Ticket, error){
		"member apply": func() (ticket.Ticket, error) {
			return svc.Apply(ctx, authz.Principal{ID: member.String()}, ev.ID)
		},
		"apply for other": func() (ticket.Ticket, error) {
			return svc.ApplyForOther(ctx, weblabLead, ev.ID, other)
		},
		"guest apply": func() (ticket.Ticket, error) {
			if _, err := svc.ApplyGuest(ctx, anonymous, ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}); err != nil {
				return ticket.Ticket{}, err
			}
			return store.GetByGuestEvent(ctx, "ada@example.com", ev.ID)
		},
		"forms answer, signed in": func() (ticket.Ticket, error) {
			if _, err := svc.RecordFormResponse(ctx, formsService, ticket.FormResponse{
				FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &answering,
			}); err != nil {
				return ticket.Ticket{}, err
			}
			return store.GetByOwnerEvent(ctx, answering, formEvent.ID)
		},
		"forms answer, guest": func() (ticket.Ticket, error) {
			if _, err := svc.RecordFormResponse(ctx, formsService, acceptedGuest(formID, "Grace", "Hopper", "grace@example.com")); err != nil {
				return ticket.Ticket{}, err
			}
			return store.GetByGuestEvent(ctx, "grace@example.com", formEvent.ID)
		},
		"guest added to a draft": func() (ticket.Ticket, error) {
			applied, err := svc.ApplyGuest(ctx, weblabLead, draft.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"})
			return applied.Ticket, err
		},
	}
	keys, codes := map[string]string{}, map[string]string{}
	for name, write := range written {
		got, err := write()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		key, code, version := f.storedKeys(t, got.ID)
		if !accessKeyShape.MatchString(key) || !codeShape.MatchString(code) || version != 1 {
			t.Fatalf("%s: stored %q %q %d", name, key, code, version)
		}
		if got.AccessKey != key || got.Code != ticket.FormatCode(code) || got.QRVersion != 1 {
			t.Fatalf("%s: answered %q %q %d, stored %q %q", name, got.AccessKey, got.Code, got.QRVersion, key, code)
		}
		if other, ok := keys[key]; ok {
			t.Fatalf("%s shares its access key with %s", name, other)
		}
		if other, ok := codes[code]; ok {
			t.Fatalf("%s shares its code with %s", name, other)
		}
		keys[key], codes[code] = name, name
	}
}

// A code another Ticket holds is drawn again: the Ticket is written with
// the next one.
func TestCreateDrawsATakenCodeAgain(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	ev, err := f.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	store := ticket.NewPostgresStore(f.pool)
	ticket.DrawCodes(store, "K7M2QX9F")
	first, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestEmail: "a@example.com"})
	if err != nil || first.Code != "K7M2-QX9F" {
		t.Fatalf("first %+v %v", first, err)
	}
	for _, notify := range []bool{false, true} {
		store.NotifyCreated(notify)
		ticket.DrawCodes(store, "K7M2QX9F", "K7M2QX9F", "AB12CD34")
		email := uuid.NewString() + "@example.com"
		second, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestEmail: email})
		if err != nil {
			t.Fatalf("notify %v: %v", notify, err)
		}
		if second.Code != "AB12-CD34" {
			t.Fatalf("notify %v: code %q", notify, second.Code)
		}
		if _, code, _ := f.storedKeys(t, second.ID); code != "AB12CD34" {
			t.Fatalf("notify %v: stored %q", notify, code)
		}
		if _, err := f.pool.Exec(ctx, `DELETE FROM tickets WHERE id = $1`, second.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The owner's or the guest e-mail's own unique index is still a
	// conflict, not a code to draw again.
	if _, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestEmail: "a@example.com"}); !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("second ticket for the e-mail: %v", err)
	}
}

// The panel's reads answer the code (XXXX-XXXX), never the access key or the
// QR version; the holder's list answers the code too.
func TestTicketAnswersCarryTheCodeNotTheAccessKey(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	store := ticket.NewPostgresStore(f.pool)
	svc := f.service(store)
	ev, err := f.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	member := f.person(t, "member@example.com")
	holder := authz.Principal{ID: member.String()}
	created, err := svc.Apply(ctx, holder, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, code, _ := f.storedKeys(t, created.ID)
	if _, err := svc.ApplyGuest(ctx, anonymous, ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}

	answers := map[string]func() (any, error){
		"GET /v1/tickets/{id}":         func() (any, error) { return svc.Get(ctx, yk, created.ID) },
		"GET /v1/tickets?userId=":      func() (any, error) { return svc.ListQuery(ctx, yk, "", &member) },
		"GET /v1/tickets?email=":       func() (any, error) { return svc.ListQuery(ctx, yk, "ada@example.com", nil) },
		"GET /v1/events/{id}/tickets":  func() (any, error) { return svc.ListByEvent(ctx, yk, ev.ID) },
		"GET /v1/tickets/me":           func() (any, error) { return svc.Mine(ctx, holder) },
		"GET /v1/tickets/user/{u}/...": func() (any, error) { return svc.GetByUserEvent(ctx, yk, member, ev.ID) },
	}
	for name, read := range answers {
		got, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if strings.Contains(text, key) || strings.Contains(strings.ToLower(text), "accesskey") || strings.Contains(strings.ToLower(text), "qrversion") {
			t.Errorf("%s answers the access key or QR version: %s", name, text)
		}
		if !strings.Contains(text, `"code":"`) {
			t.Errorf("%s answers no code: %s", name, text)
		}
	}
	got, err := svc.Get(ctx, yk, created.ID)
	if err != nil || got.Code != ticket.FormatCode(code) {
		t.Fatalf("code %q, stored %q: %v", got.Code, code, err)
	}
}
