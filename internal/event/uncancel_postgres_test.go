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

// Uncancel publishes the Event again with the cancellation cleared and its
// publication kept, writes the legacy active as the derived one, and brings
// back the Competitions its cancellation cancelled, and only those (ticket
// 15).
func TestPostgresStoreUncancel(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	future := time.Now().Add(48 * time.Hour)
	ev, err := store.Create(ctx, event.Event{Name: "Yayında", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &future})
	if err != nil {
		t.Fatal(err)
	}
	competitions := map[string]uuid.UUID{}
	for _, c := range []struct{ name, status, reason string }{
		{"açık", "open", ""}, {"kapandı", "closed", ""}, {"kendi iptali", "cancelled", "Kendi gerekçesi"},
	} {
		id := uuid.New()
		competitions[c.name] = id
		if _, err := pool.Exec(ctx, `INSERT INTO competitions (id, event_id, name, status, cancel_reason) VALUES ($1, $2, $3, $4, $5)`,
			id, ev.ID, c.name, c.status, c.reason); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := store.Uncancel(ctx, ev.ID); err != nil || changed {
		t.Fatalf("uncancel a published Event: %v %v", changed, err)
	}
	if changed, err := store.Cancel(ctx, ev.ID, nil, "Ertelendi"); err != nil || !changed {
		t.Fatalf("cancel: %v %v", changed, err)
	}
	if changed, err := store.Uncancel(ctx, ev.ID); err != nil || !changed {
		t.Fatalf("uncancel: %v %v", changed, err)
	}
	var status, reason string
	var active bool
	var cancelledAt, publishedAt *time.Time
	var cancelledBy *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT status, active, cancelled_at, cancelled_by, cancel_reason, published_at FROM events WHERE id = $1`, ev.ID).
		Scan(&status, &active, &cancelledAt, &cancelledBy, &reason, &publishedAt); err != nil {
		t.Fatal(err)
	}
	if status != "published" || !active || cancelledAt != nil || cancelledBy != nil || reason != "" || publishedAt == nil || !publishedAt.Equal(*ev.PublishedAt) {
		t.Fatalf("event: %s active=%v cancelled=%v/%v %q published=%v", status, active, cancelledAt, cancelledBy, reason, publishedAt)
	}
	for name, want := range map[string]struct{ status, reason string }{
		"açık": {"open", ""}, "kapandı": {"closed", ""}, "kendi iptali": {"cancelled", "Kendi gerekçesi"},
	} {
		var status, reason string
		var before *string
		var withEvent *time.Time
		if err := pool.QueryRow(ctx, `SELECT status, cancel_reason, status_before_event_cancel, cancelled_with_event_at FROM competitions WHERE id = $1`,
			competitions[name]).Scan(&status, &reason, &before, &withEvent); err != nil {
			t.Fatal(err)
		}
		if status != want.status || reason != want.reason || before != nil || withEvent != nil {
			t.Errorf("%s: %s %q before=%v withEvent=%v", name, status, reason, before, withEvent)
		}
	}

	// An Event that ended while cancelled comes back published and not
	// active, in the legacy column too.
	past := time.Now().Add(-72 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE events SET status = 'cancelled', start_date = $2, cancelled_at = now() WHERE id = $1`, ev.ID, past); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Uncancel(ctx, ev.ID); err != nil || !changed {
		t.Fatalf("uncancel ended: %v %v", changed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, active FROM events WHERE id = $1`, ev.ID).Scan(&status, &active); err != nil || status != "published" || active {
		t.Fatalf("ended: %s %v %v", status, active, err)
	}
}
