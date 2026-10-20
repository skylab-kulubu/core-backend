package event_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The status mails are queued with the cancellation and its undoing, in
// their own write, and the queue claims, records, retries and finishes them
// (ticket 14).
func TestPostgresStatusMailQueue(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	start := time.Now().Add(48 * time.Hour)
	ev, err := store.Create(ctx, event.Event{Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	ticketID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.com')`, ticketID, ev.ID); err != nil {
		t.Fatal(err)
	}

	// Undoing a cancellation that was not mailed queues nothing.
	if changed, err := store.Cancel(ctx, ev.ID, nil, "", false); err != nil || !changed {
		t.Fatalf("cancel: %v %v", changed, err)
	}
	if changed, err := store.Uncancel(ctx, ev.ID, true); err != nil || !changed {
		t.Fatalf("uncancel: %v %v", changed, err)
	}
	if _, ok, err := store.ClaimStatusMail(ctx, time.Now(), time.Minute); err != nil || ok {
		t.Fatalf("nothing queued: %v %v", ok, err)
	}

	if changed, err := store.Cancel(ctx, ev.ID, nil, "Hava", true); err != nil || !changed {
		t.Fatalf("cancel notified: %v %v", changed, err)
	}
	read, err := store.Get(ctx, ev.ID)
	if err != nil || read.CancelNotifiedAt == nil {
		t.Fatalf("cancelNotifiedAt %+v %v", read.CancelNotifiedAt, err)
	}
	claimed, ok, err := store.ClaimStatusMail(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil || !ok || claimed.Kind != event.StatusMailCancelled || claimed.EventID != ev.ID || claimed.Follows != nil {
		t.Fatalf("claim %+v %v %v", claimed, ok, err)
	}
	if _, ok, err := store.ClaimStatusMail(ctx, time.Now().Add(time.Second), time.Minute); err != nil || ok {
		t.Fatalf("held by its lease: %v %v", ok, err)
	}
	if err := store.RetryStatusMail(ctx, claimed, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = store.ClaimStatusMail(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil || !ok || claimed.Attempts != 1 {
		t.Fatalf("after retry %+v %v %v", claimed, ok, err)
	}
	for range 2 {
		if err := store.RecordStatusMailSend(ctx, claimed.ID, ticketID); err != nil {
			t.Fatal(err)
		}
	}
	if sends, err := store.StatusMailSends(ctx, claimed.ID); err != nil || len(sends) != 1 || sends[0] != ticketID {
		t.Fatalf("sends %v %v", sends, err)
	}
	if err := store.FinishStatusMail(ctx, claimed, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The undo queues the reinstated mail after it and clears the mark.
	if changed, err := store.Uncancel(ctx, ev.ID, true); err != nil || !changed {
		t.Fatalf("uncancel notified: %v %v", changed, err)
	}
	if read, err = store.Get(ctx, ev.ID); err != nil || read.CancelNotifiedAt != nil {
		t.Fatalf("after uncancel %+v %v", read.CancelNotifiedAt, err)
	}
	reinstated, ok, err := store.ClaimStatusMail(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil || !ok || reinstated.Kind != event.StatusMailReinstated || reinstated.Follows == nil || *reinstated.Follows != claimed.ID {
		t.Fatalf("reinstated %+v %v %v", reinstated, ok, err)
	}
}
