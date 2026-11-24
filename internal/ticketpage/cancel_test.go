package ticketpage_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
)

// The holder cancels from the page with its key (kontenjan-iptal-hatirlatma
// 03): the answer names the state only; the Ticket is cancelled by the
// holder and the holder is told; again answers the same with no second
// mail. The page says cancellable until the Event starts, and nothing of
// the person is in the answer.
func TestHolderCancelsFromThePage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	g := f.guest("elif@example.com")
	view, err := f.svc.Page(ctx, g.AccessKey)
	if err != nil || !view.Cancellable {
		t.Fatalf("before: %+v %v", view, err)
	}
	got, err := f.svc.Cancel(ctx, g.AccessKey)
	if err != nil || got.State != ticketpage.StateTicketCancelled {
		t.Fatalf("cancel %+v %v", got, err)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != `{"state":"ticket_cancelled"}` {
		t.Fatalf("answer %s", raw)
	}
	stored, _ := f.store.Get(ctx, g.ID)
	if stored.StatusOf() != ticket.StatusCancelled || stored.CancelledBy != ticket.CancelledByHolder || f.store.CancelMails[g.ID] != 1 {
		t.Fatalf("stored %+v (mails %v)", stored, f.store.CancelMails)
	}
	if again, err := f.svc.Cancel(ctx, g.AccessKey); err != nil || again.State != ticketpage.StateTicketCancelled || f.store.CancelMails[g.ID] != 1 {
		t.Fatalf("again %+v %v", again, err)
	}
	view, err = f.svc.Page(ctx, g.AccessKey)
	if err != nil || view.Cancellable || view.State != ticketpage.StateTicketCancelled {
		t.Fatalf("after: %+v %v", view, err)
	}
	if strings.Contains(string(raw), "Elif") {
		t.Fatal("the answer names the holder")
	}
}

// The last moment on the page: a minute before the start the holder may
// cancel; a minute after, 409 ticket_cancel_closed and the page says not
// cancellable. A Ticket with a check-in stays.
func TestPageCancelClosesAtTheStart(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	early := f.guest("early@example.com")
	late := f.guest("late@example.com")

	f.now = f.ev.StartDate.Add(-time.Minute)
	if _, err := f.svc.Cancel(ctx, early.AccessKey); err != nil {
		t.Fatalf("a minute before: %v", err)
	}
	f.now = f.ev.StartDate.Add(time.Minute)
	if view, err := f.svc.Page(ctx, late.AccessKey); err != nil || view.Cancellable {
		t.Fatalf("page after the start: %+v %v", view, err)
	}
	if _, err := f.svc.Cancel(ctx, late.AccessKey); !errors.Is(err, ticket.ErrCancelClosed) {
		t.Fatalf("a minute after: %v", err)
	}
	stored, _ := f.store.Get(ctx, late.ID)
	if stored.StatusOf() != ticket.StatusActive {
		t.Fatalf("late %+v", stored)
	}
}

// A malformed key never reaches the store; an unknown one is ErrNotFound.
func TestPageCancelUnknownKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	before := f.store.lookups.Load()
	for _, key := range []string{"", "short", "../../../../etc/passwd"} {
		if _, err := f.svc.Cancel(t.Context(), key); !errors.Is(err, ticketpage.ErrNotFound) {
			t.Fatalf("%q: %v", key, err)
		}
	}
	if f.store.lookups.Load() != before {
		t.Fatal("a malformed key reached the store")
	}
	if _, err := f.svc.Cancel(t.Context(), "ABCDEFGHIJKLMNOPQRSTUVWXYZ"); !errors.Is(err, ticketpage.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}
