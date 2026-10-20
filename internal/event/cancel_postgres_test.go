package event_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Cancel changes only a current, published Event that has not ended, writes
// the legacy active false, and cancels its Competitions with it, each with
// its earlier status; a Competition cancelled on its own or archived is
// left as it is (tickets 04 and 15).
func TestPostgresStoreCancel(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	actor := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, actor, user.Profile{Email: "iptal@example.com"}); err != nil {
		t.Fatal(err)
	}
	future, past := time.Now().Add(48*time.Hour), time.Now().Add(-72*time.Hour)
	create := func(name string, status event.Status, start *time.Time) event.Event {
		created, err := store.Create(ctx, event.Event{Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB", Status: status, StartDate: start})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	published := create("Yayında", event.StatusPublished, &future)
	competitions := map[string]uuid.UUID{}
	for _, c := range []struct{ name, status, reason, archived string }{
		{"taslak", "draft", "", ""}, {"açık", "open", "", ""}, {"sürüyor", "running", "", ""}, {"kapandı", "closed", "", ""},
		{"kendi iptali", "cancelled", "Kendi gerekçesi", ""}, {"arşivli", "open", "", "now()"},
	} {
		id := uuid.New()
		competitions[c.name] = id
		archived := "NULL"
		if c.archived != "" {
			archived = c.archived
		}
		if _, err := pool.Exec(ctx, `INSERT INTO competitions (id, event_id, name, status, cancel_reason, archived_at)
			VALUES ($1, $2, $3, $4, $5, `+archived+`)`, id, published.ID, c.name, c.status, c.reason); err != nil {
			t.Fatal(err)
		}
	}

	changed, err := store.Cancel(ctx, published.ID, &actor, "", false)
	if err != nil || !changed {
		t.Fatalf("cancel: %v %v", changed, err)
	}
	var status, reason string
	var active bool
	var cancelledAt *time.Time
	var cancelledBy *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT status, active, cancelled_at, cancelled_by, cancel_reason FROM events WHERE id = $1`, published.ID).
		Scan(&status, &active, &cancelledAt, &cancelledBy, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || active || cancelledAt == nil || cancelledBy == nil || *cancelledBy != actor || reason != "" {
		t.Fatalf("event: %s active=%v at=%v by=%v %q", status, active, cancelledAt, cancelledBy, reason)
	}
	read, err := store.Get(ctx, published.ID)
	if err != nil || read.Status != event.StatusCancelled || read.Active || read.CancelledAt == nil {
		t.Fatalf("read %+v %v", read, err)
	}
	for name, want := range map[string]struct {
		status, reason, before string
		withEvent              bool
	}{
		"taslak":       {"cancelled", event.CompetitionCancelReason, "draft", true},
		"açık":         {"cancelled", event.CompetitionCancelReason, "open", true},
		"sürüyor":      {"cancelled", event.CompetitionCancelReason, "running", true},
		"kapandı":      {"cancelled", event.CompetitionCancelReason, "closed", true},
		"kendi iptali": {"cancelled", "Kendi gerekçesi", "", false},
		"arşivli":      {"open", "", "", false},
	} {
		var status, reason string
		var before *string
		var withEvent *time.Time
		if err := pool.QueryRow(ctx, `SELECT status, cancel_reason, status_before_event_cancel, cancelled_with_event_at FROM competitions WHERE id = $1`,
			competitions[name]).Scan(&status, &reason, &before, &withEvent); err != nil {
			t.Fatal(err)
		}
		gotBefore := ""
		if before != nil {
			gotBefore = *before
		}
		if status != want.status || reason != want.reason || gotBefore != want.before || (withEvent != nil) != want.withEvent {
			t.Errorf("%s: %s %q before=%q withEvent=%v", name, status, reason, gotBefore, withEvent)
		}
	}

	// Once cancelled, again changes nothing; so do a draft and an ended
	// Event.
	for _, ev := range []event.Event{published, create("Taslak", event.StatusDraft, &future), create("Bitti", event.StatusPublished, &past)} {
		if changed, err := store.Cancel(ctx, ev.ID, nil, "x", false); err != nil || changed {
			t.Fatalf("%s: %v %v", ev.Name, changed, err)
		}
	}

	// The reason is edited by PATCH while cancelled, and only then.
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	leader := authz.Principal{ID: actor.String(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	var reasonPatch event.Patch
	if err := json.Unmarshal([]byte(`{"cancelReason":"Ertelendi"}`), &reasonPatch); err != nil {
		t.Fatal(err)
	}
	if edited, err := svc.Patch(ctx, leader, published.ID, reasonPatch); err != nil || edited.CancelReason != "Ertelendi" {
		t.Fatalf("edit reason: %+v %v", edited.CancelReason, err)
	}
	open := create("Açık", event.StatusPublished, &future)
	if edited, err := svc.Patch(ctx, leader, open.ID, reasonPatch); err != nil || edited.CancelReason != "" {
		t.Fatalf("reason on a published Event: %q %v", edited.CancelReason, err)
	}

	// A reason goes to the Competitions too.
	withReason := create("Gerekçeli", event.StatusPublished, nil)
	competition := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO competitions (id, event_id, name) VALUES ($1, $2, 'yarışma')`, competition, withReason.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Cancel(ctx, withReason.ID, nil, "Hava koşulları", false); err != nil || !changed {
		t.Fatalf("undated: %v %v", changed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT cancel_reason FROM competitions WHERE id = $1`, competition).Scan(&reason); err != nil || reason != "Hava koşulları" {
		t.Fatalf("competition reason %q %v", reason, err)
	}
}
