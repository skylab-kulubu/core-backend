package event_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// Two Events that take the same application form at once: the one-place
// rule runs again inside each write under the form's lock, so one wins
// (ADR 0069, addendum of 2026-11-22). A form two Events listed before the
// rule stays on both, and their writes go through.
func TestApplicationFormOnOneEventInPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	shared := formURL(uuid.New())
	var legacy []event.Event
	for _, name := range []string{"Eski", "Yeni"} {
		e, err := store.Create(ctx, event.Event{Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusPublished, FormURL: shared})
		if err != nil {
			t.Fatal(err)
		}
		legacy = append(legacy, e)
	}
	for _, e := range legacy {
		if _, err := svc.Patch(ctx, copyLeader, e.ID, patchOf(t, `{"formUrl":"`+shared+`","description":"güncel"}`)); err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
	}
	if _, err := svc.Create(ctx, copyLeader, event.Event{Name: "Üçüncü", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: shared}); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("third: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION slow_event_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(0.3); RETURN NEW; END $$;
		CREATE TRIGGER slow_event_write BEFORE UPDATE ON events FOR EACH ROW EXECUTE FUNCTION slow_event_write();`); err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for _, name := range []string{"Birinci", "İkinci"} {
		e, err := svc.Create(ctx, copyLeader, event.Event{Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	form := formURL(uuid.New())
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = svc.Patch(ctx, copyLeader, id, patchOf(t, `{"formUrl":"`+form+`"}`))
		}()
	}
	wg.Wait()
	won, lost := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, event.ErrFormInUse):
			lost++
		default:
			t.Fatal(err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("won %d, lost %d: %v", won, lost, errs)
	}
}

// Restoring an archived Event and binding its form to another Event at
// once: the restore takes the same form locks and checks again inside its
// write, so one of them wins (review 360-2, 2026-11-22). A trigger holds
// each write open, which without the lock lets both checks pass.
func TestRestoreAndBindingAreDecidedOneAtATime(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	for round := range 3 {
		form := formURL(uuid.New())
		archived, err := svc.Create(ctx, copyLeader, event.Event{Name: "Eski", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, FormURL: form})
		if err != nil {
			t.Fatal(err)
		}
		// Archived by nobody: the test's caller has no account row.
		if err := store.Archive(ctx, archived.ID, nil); err != nil {
			t.Fatal(err)
		}
		other, err := svc.Create(ctx, copyLeader, event.Event{Name: "Yeni", Location: "YTÜ", OwnerTeam: "WEBLAB"})
		if err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			if _, err := pool.Exec(ctx, `
				CREATE FUNCTION slow_event_write() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN PERFORM pg_sleep(0.3); RETURN NEW; END $$;
				CREATE TRIGGER slow_event_write BEFORE UPDATE ON events FOR EACH ROW EXECUTE FUNCTION slow_event_write();`); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errs[0] = svc.Restore(ctx, copyLeader, archived.ID)
		}()
		go func() {
			defer wg.Done()
			_, errs[1] = svc.Patch(ctx, copyLeader, other.ID, patchOf(t, `{"formUrl":"`+form+`"}`))
		}()
		wg.Wait()
		won, lost := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, event.ErrFormInUse):
				lost++
			default:
				t.Fatal(err)
			}
		}
		if won != 1 || lost != 1 {
			t.Fatalf("round %d: won %d, lost %d: %v", round, won, lost, errs)
		}
	}
}
