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
	"github.com/skylab-kulubu/core-backend/internal/optional"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// Two writes that put the same form in two places at once are decided one
// after the other (review B1): the one-place rule runs again inside each
// write under a lock per form, so exactly one of them wins. A trigger holds
// each write open, which without the lock lets both checks pass.
func TestFormPlacesAreDecidedOneWriteAtATime(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION slow_event_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(0.3); RETURN NEW; END $$;
		CREATE TRIGGER slow_event_write BEFORE UPDATE ON events FOR EACH ROW EXECUTE FUNCTION slow_event_write();`); err != nil {
		t.Fatal(err)
	}
	svc := event.NewService(event.NewPostgresStore(pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	first, err := svc.Create(ctx, copyLeader, event.Event{Name: "Birinci", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Create(ctx, copyLeader, event.Event{Name: "İkinci", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	form := formURL(uuid.New())
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, write := range []struct {
		id   uuid.UUID
		body string
	}{
		{first.ID, `{"feedbackFormUrl":"` + form + `"}`},
		{second.ID, `{"formUrl":"` + form + `"}`},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = svc.Patch(ctx, copyLeader, write.id, patchOf(t, write.body))
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

// The same for two Sessions, and a Session against an Event: one wins.
func TestSessionFormPlacesAreDecidedOneWriteAtATime(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION slow_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(0.3); RETURN NEW; END $$;
		CREATE TRIGGER slow_session_write BEFORE UPDATE ON sessions FOR EACH ROW EXECUTE FUNCTION slow_write();
		CREATE TRIGGER slow_event_write BEFORE UPDATE ON events FOR EACH ROW EXECUTE FUNCTION slow_write();`); err != nil {
		t.Fatal(err)
	}
	svc := event.NewService(event.NewPostgresStore(pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	ev, err := svc.Create(ctx, copyLeader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", FeedbackMode: event.FeedbackPerSession})
	if err != nil {
		t.Fatal(err)
	}
	day, err := svc.CreateDay(ctx, copyLeader, event.Day{EventID: ev.ID, Name: "Gün"})
	if err != nil {
		t.Fatal(err)
	}
	session := func(title string) event.Session {
		created, err := svc.CreateSession(ctx, copyLeader, event.Session{EventDayID: day.ID, Title: title, SpeakerName: "Ada", SessionType: "talk"})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	race := func(writes ...func(url string) error) (won, lost int) {
		t.Helper()
		url := formURL(uuid.New())
		var wg sync.WaitGroup
		errs := make([]error, len(writes))
		for i, write := range writes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = write(url)
			}()
		}
		wg.Wait()
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
		return won, lost
	}
	setForm := func(sess event.Session) func(string) error {
		return func(url string) error {
			_, err := svc.UpdateSession(ctx, copyLeader, sess.ID, sess, nil, event.SessionFeedback{FormURL: optional.Field[string]{Set: true, Value: url}})
			return err
		}
	}
	if won, lost := race(setForm(session("A")), setForm(session("B"))); won != 1 || lost != 1 {
		t.Fatalf("two Sessions: won %d, lost %d", won, lost)
	}
	eventForm := func(url string) error {
		_, err := svc.Patch(ctx, copyLeader, ev.ID, patchOf(t, `{"feedbackFormUrl":"`+url+`"}`))
		return err
	}
	if won, lost := race(setForm(session("C")), eventForm); won != 1 || lost != 1 {
		t.Fatalf("a Session and its Event: won %d, lost %d", won, lost)
	}
}
