package account_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/apppref"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// appPreferencesErased is an App preference eraser with nothing to delete.
type appPreferencesErased struct{}

func (appPreferencesErased) EraseSubject(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}

// sagaAppPreferences records the erase_app_preferences step in the saga's
// order.
type sagaAppPreferences struct {
	events *sagaEvents
	err    error
}

func (p sagaAppPreferences) EraseSubject(context.Context, uuid.UUID) (int64, error) {
	p.events.add("erase_app_preferences")
	return 1, p.err
}

// Right after the communication preferences and before any service.
func TestErasureSagaErasesTheAppPreferencesBeforeAnyService(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	if worked, err := f.run(f.saga()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	events := f.events.list()
	apps := slices.Index(events, "erase_app_preferences")
	communication := slices.Index(events, "erase_communication_preferences")
	if apps < 0 || communication < 0 || apps < communication {
		t.Fatalf("events = %v", events)
	}
	for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core", "delete_identity"} {
		if index := slices.Index(events, later); index >= 0 && index < apps {
			t.Fatalf("%s before erase_app_preferences: %v", later, events)
		}
	}
	if !slices.Contains(f.checkpoints(), user.DeletionStepEraseAppPreferences) {
		t.Fatalf("no checkpoint for erase_app_preferences: %v", f.checkpoints())
	}
}

// A failing or missing eraser stops the saga before the services, and the
// step is not checkpointed.
func TestErasureSagaStopsWhenTheAppPreferencesCannotBeErased(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		eraser func(*sagaFixture) account.AppPreferenceEraser
	}{
		{name: "database down", eraser: func(f *sagaFixture) account.AppPreferenceEraser {
			return sagaAppPreferences{events: f.events, err: errors.New("app preferences: erase: connection refused")}
		}},
		{name: "no eraser", eraser: func(*sagaFixture) account.AppPreferenceEraser { return nil }},
	} {
		f := newSagaFixture(t, user.NewMemoryStore())
		config := f.config
		config.Services = f.group()
		config.ContactConsents = f.consents()
		config.SkyPassWallet = sagaWallet{events: f.events}
		config.CommunicationPreferences = sagaCommunicationPreferences{events: f.events}
		config.AppPreferences = tc.eraser(f)
		worked, err := f.run(newWorkerWithoutConsents(f, config))
		if !worked || err == nil || !strings.Contains(err.Error(), "erase_app_preferences_failed") {
			t.Fatalf("%s: worked=%v err=%v", tc.name, worked, err)
		}
		for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core", "delete_identity"} {
			if f.events.count(later) != 0 {
				t.Fatalf("%s: the saga went on to %s: %v", tc.name, later, f.events.list())
			}
		}
		state := f.state()
		if state.Status != user.DeletionRequestPending || state.LastErrorCode != "erase_app_preferences_failed" {
			t.Fatalf("%s: request = %+v", tc.name, state)
		}
		if slices.Contains(f.checkpoints(), user.DeletionStepEraseAppPreferences) {
			t.Fatalf("%s: checkpointed a failed erasure", tc.name)
		}
		f.assertNoPersonalData(state.LastErrorCode)
	}
}

// The real eraser in the saga: the person's rows in every app go, another
// person's stay, and a second pass finds nothing and succeeds. It erases
// with App preferences off too.
func TestErasureSagaStepDeletesThePersonsAppPreferences(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	store := apppref.NewMemoryStore()
	other := uuid.New()
	for _, person := range []uuid.UUID{f.subjectID, other} {
		for _, app := range []string{"forms", "panel"} {
			w := apppref.Write{Value: json.RawMessage(`{"details":false}`), SchemaVersion: 1}
			if _, err := store.Put(context.Background(), person, app, "list", w, "skyforms", time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	prefs := apppref.NewService(apppref.Config{}, store)
	config := f.config
	config.Services = f.group()
	config.ContactConsents = f.consents()
	config.SkyPassWallet = sagaWallet{events: f.events}
	config.CommunicationPreferences = sagaCommunicationPreferences{events: f.events}
	config.AppPreferences = prefs
	if worked, err := f.run(newWorkerWithoutConsents(f, config)); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if sums, _ := store.Summaries(context.Background(), f.subjectID); len(sums) != 0 {
		t.Fatalf("rows left: %+v", sums)
	}
	if sums, _ := store.Summaries(context.Background(), other); len(sums) != 2 {
		t.Fatalf("another person's rows: %+v", sums)
	}
	if n, err := prefs.EraseSubject(context.Background(), f.subjectID); err != nil || n != 0 {
		t.Fatalf("again: %d %v", n, err)
	}
}
