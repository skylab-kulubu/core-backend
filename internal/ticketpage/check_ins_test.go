package ticketpage_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
)

// entries answers PageCheckIns from a map, counting the lookups.
type entries struct {
	byKey   map[string]ticket.PageCheckIns
	err     error
	lookups int
}

func (e *entries) PageCheckIns(_ context.Context, key string) (ticket.PageCheckIns, error) {
	e.lookups++
	if e.err != nil {
		return ticket.PageCheckIns{}, e.err
	}
	got, ok := e.byKey[key]
	if !ok {
		return ticket.PageCheckIns{}, ticket.ErrNotFound
	}
	return got, nil
}

const pollKey = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func pollService(e *entries, now time.Time) *ticketpage.Service {
	svc := ticketpage.New(ticket.NewMemoryStore(), event.NewMemoryStore(), nil, nil, nil, nil, ticketpage.Config{
		Page: ticket.PageConfig{Grace: ticket.DefaultPageGrace},
	})
	svc.Now = func() time.Time { return now }
	svc.Entries = e
	return svc
}

// The page polls its own check-ins: the Session's title and when, and
// nothing else (no ids, no names).
func TestCheckInsAnswersTheTicketsOwn(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 12, 12, 7, 0, 0, 0, time.UTC)
	end := start.Add(8 * time.Hour)
	at := start.Add(5 * time.Minute)
	e := &entries{byKey: map[string]ticket.PageCheckIns{pollKey: {
		Held:     true,
		Event:    event.Event{Status: event.StatusPublished, StartDate: &start, EndDate: &end},
		CheckIns: []ticket.PageCheckIn{{Session: "Açılış", At: at}},
	}}}
	view, err := pollService(e, start).CheckIns(t.Context(), pollKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"checkIns":[{"session":"Açılış","at":"2026-12-12T07:05:00Z"}]}`; string(raw) != want {
		t.Fatalf("json %s, want %s", raw, want)
	}

	// None yet: an empty list, never null.
	e.byKey[pollKey] = ticket.PageCheckIns{Held: true, Event: event.Event{Status: event.StatusPublished, StartDate: &start, EndDate: &end}}
	view, err = pollService(e, start).CheckIns(t.Context(), pollKey)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(view); string(raw) != `{"checkIns":[]}` {
		t.Fatalf("none yet: %s", raw)
	}
}

// The page's own rules: 404 for a malformed key (the store is not asked),
// an unknown one, a Ticket without its holder and a draft's; 410 a week
// after the end; no store at all is 404.
func TestCheckInsFollowsThePagesRules(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 12, 12, 7, 0, 0, 0, time.UTC)
	end := start.Add(8 * time.Hour)
	published := event.Event{Status: event.StatusPublished, StartDate: &start, EndDate: &end}

	e := &entries{byKey: map[string]ticket.PageCheckIns{}}
	if _, err := pollService(e, start).CheckIns(t.Context(), "nope"); !errors.Is(err, ticketpage.ErrNotFound) || e.lookups != 0 {
		t.Fatalf("malformed: %v, %d lookups", err, e.lookups)
	}
	if _, err := pollService(e, start).CheckIns(t.Context(), pollKey); !errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}

	e.byKey[pollKey] = ticket.PageCheckIns{Held: false, Event: published}
	if _, err := pollService(e, start).CheckIns(t.Context(), pollKey); !errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("no holder: %v", err)
	}

	e.byKey[pollKey] = ticket.PageCheckIns{Held: true, Event: event.Event{Status: event.StatusDraft, StartDate: &start, EndDate: &end}}
	if _, err := pollService(e, start).CheckIns(t.Context(), pollKey); !errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("draft: %v", err)
	}

	e.byKey[pollKey] = ticket.PageCheckIns{Held: true, Event: published}
	if _, err := pollService(e, end.Add(6*24*time.Hour)).CheckIns(t.Context(), pollKey); err != nil {
		t.Fatalf("six days after: %v", err)
	}
	if _, err := pollService(e, end.Add(7*24*time.Hour)).CheckIns(t.Context(), pollKey); !errors.Is(err, ticketpage.ErrGone) {
		t.Fatalf("a week after: %v", err)
	}

	// A cancelled Event's page still answers (with what it has).
	e.byKey[pollKey] = ticket.PageCheckIns{Held: true, Event: event.Event{Status: event.StatusCancelled, StartDate: &start, EndDate: &end}}
	if _, err := pollService(e, start).CheckIns(t.Context(), pollKey); err != nil {
		t.Fatalf("cancelled: %v", err)
	}

	failing := &entries{err: errors.New("db down")}
	if _, err := pollService(failing, start).CheckIns(t.Context(), pollKey); err == nil || errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("store error: %v", err)
	}

	none := pollService(nil, start)
	none.Entries = nil
	if _, err := none.CheckIns(t.Context(), pollKey); !errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("no store: %v", err)
	}
}
