package account_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// communicationPreferencesErased is a preference eraser with nothing to delete.
type communicationPreferencesErased struct{}

func (communicationPreferencesErased) EraseSubject(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}

// sagaCommunicationPreferences records the erase_communication_preferences step in the saga's
// order.
type sagaCommunicationPreferences struct {
	events *sagaEvents
	err    error
}

func (p sagaCommunicationPreferences) EraseSubject(context.Context, uuid.UUID) (int64, error) {
	p.events.add("erase_communication_preferences")
	return 1, p.err
}

// Right after the Wallet step and before any service: core's own data waits
// for no one.
func TestErasureSagaErasesTheCommunicationPreferencesBeforeAnyService(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	if worked, err := f.run(f.saga()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	events := f.events.list()
	prefs := slices.Index(events, "erase_communication_preferences")
	wallet := slices.Index(events, "erase_skypass_wallet")
	if prefs < 0 || wallet < 0 || prefs < wallet {
		t.Fatalf("events = %v", events)
	}
	for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core", "delete_identity"} {
		if index := slices.Index(events, later); index >= 0 && index < prefs {
			t.Fatalf("%s before erase_communication_preferences: %v", later, events)
		}
	}
	if !slices.Contains(f.checkpoints(), user.DeletionStepEraseCommunicationPreferences) {
		t.Fatalf("no checkpoint for erase_communication_preferences: %v", f.checkpoints())
	}
}

// A failing or missing eraser stops the saga before the services, and the
// step is not checkpointed.
func TestErasureSagaStopsWhenTheCommunicationPreferencesCannotBeErased(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		eraser func(*sagaFixture) account.CommunicationPreferenceEraser
	}{
		{name: "database down", eraser: func(f *sagaFixture) account.CommunicationPreferenceEraser {
			return sagaCommunicationPreferences{events: f.events, err: errors.New("communication preferences: erase: connection refused")}
		}},
		{name: "no eraser", eraser: func(*sagaFixture) account.CommunicationPreferenceEraser { return nil }},
	} {
		f := newSagaFixture(t, user.NewMemoryStore())
		config := f.config
		config.Services = f.group()
		config.ContactConsents = f.consents()
		config.SkyPassWallet = sagaWallet{events: f.events}
		config.CommunicationPreferences = tc.eraser(f)
		worked, err := f.run(newWorkerWithoutConsents(f, config))
		if !worked || err == nil || !strings.Contains(err.Error(), "erase_communication_preferences_failed") {
			t.Fatalf("%s: worked=%v err=%v", tc.name, worked, err)
		}
		for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core", "delete_identity"} {
			if f.events.count(later) != 0 {
				t.Fatalf("%s: the saga went on to %s: %v", tc.name, later, f.events.list())
			}
		}
		state := f.state()
		if state.Status != user.DeletionRequestPending || state.LastErrorCode != "erase_communication_preferences_failed" {
			t.Fatalf("%s: request = %+v", tc.name, state)
		}
		if slices.Contains(f.checkpoints(), user.DeletionStepEraseCommunicationPreferences) {
			t.Fatalf("%s: checkpointed a failed erasure", tc.name)
		}
		f.assertNoPersonalData(state.LastErrorCode)
	}
}

// The real eraser in the saga: the person's rows go, another person's stay,
// and a second pass over the step finds nothing and succeeds.
func TestErasureSagaStepDeletesThePersonsPreferences(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	store := commpref.NewMemoryStore()
	other := uuid.New()
	off := false
	for _, person := range []uuid.UUID{f.subjectID, other} {
		if err := store.Apply(context.Background(), person, []commpref.Change{{Channel: commpref.ChannelEmail, Category: commpref.Master(), Enabled: &off}, {Channel: commpref.ChannelEmail, Category: commpref.Club(commpref.KindNews), Enabled: &off}}, commpref.SourceAccountCenter, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	prefs := commpref.NewService(commpref.Config{}, store, nil, nil)
	config := f.config
	config.Services = f.group()
	config.ContactConsents = f.consents()
	config.SkyPassWallet = sagaWallet{events: f.events}
	config.CommunicationPreferences = prefs
	if worked, err := f.run(newWorkerWithoutConsents(f, config)); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if rows, _ := store.List(context.Background(), f.subjectID); len(rows) != 0 {
		t.Fatalf("rows left: %+v", rows)
	}
	if rows, _ := store.List(context.Background(), other); len(rows) != 2 {
		t.Fatalf("another person's rows: %+v", rows)
	}
	if n, err := prefs.EraseSubject(context.Background(), f.subjectID); err != nil || n != 0 {
		t.Fatalf("again: %d %v", n, err)
	}
}
