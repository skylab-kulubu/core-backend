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
	"github.com/skylab-kulubu/core-backend/internal/mailpref"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// mailPreferencesErased is a preference eraser with nothing to delete.
type mailPreferencesErased struct{}

func (mailPreferencesErased) EraseSubject(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}

// sagaMailPreferences records the erase_mail_preferences step in the saga's
// order.
type sagaMailPreferences struct {
	events *sagaEvents
	err    error
}

func (p sagaMailPreferences) EraseSubject(context.Context, uuid.UUID) (int64, error) {
	p.events.add("erase_mail_preferences")
	return 1, p.err
}

// Right after the Wallet step and before any service: core's own data waits
// for no one.
func TestErasureSagaErasesTheMailPreferencesBeforeAnyService(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	if worked, err := f.run(f.saga()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	events := f.events.list()
	prefs := slices.Index(events, "erase_mail_preferences")
	wallet := slices.Index(events, "erase_skypass_wallet")
	if prefs < 0 || wallet < 0 || prefs < wallet {
		t.Fatalf("events = %v", events)
	}
	for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core", "delete_identity"} {
		if index := slices.Index(events, later); index >= 0 && index < prefs {
			t.Fatalf("%s before erase_mail_preferences: %v", later, events)
		}
	}
	if !slices.Contains(f.checkpoints(), user.DeletionStepEraseMailPreferences) {
		t.Fatalf("no checkpoint for erase_mail_preferences: %v", f.checkpoints())
	}
}

// A failing or missing eraser stops the saga before the services, and the
// step is not checkpointed.
func TestErasureSagaStopsWhenTheMailPreferencesCannotBeErased(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		eraser func(*sagaFixture) account.MailPreferenceEraser
	}{
		{name: "database down", eraser: func(f *sagaFixture) account.MailPreferenceEraser {
			return sagaMailPreferences{events: f.events, err: errors.New("mail preferences: erase: connection refused")}
		}},
		{name: "no eraser", eraser: func(*sagaFixture) account.MailPreferenceEraser { return nil }},
	} {
		f := newSagaFixture(t, user.NewMemoryStore())
		config := f.config
		config.Services = f.group()
		config.ContactConsents = f.consents()
		config.SkyPassWallet = sagaWallet{events: f.events}
		config.MailPreferences = tc.eraser(f)
		worked, err := f.run(newWorkerWithoutConsents(f, config))
		if !worked || err == nil || !strings.Contains(err.Error(), "erase_mail_preferences_failed") {
			t.Fatalf("%s: worked=%v err=%v", tc.name, worked, err)
		}
		for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core", "delete_identity"} {
			if f.events.count(later) != 0 {
				t.Fatalf("%s: the saga went on to %s: %v", tc.name, later, f.events.list())
			}
		}
		state := f.state()
		if state.Status != user.DeletionRequestPending || state.LastErrorCode != "erase_mail_preferences_failed" {
			t.Fatalf("%s: request = %+v", tc.name, state)
		}
		if slices.Contains(f.checkpoints(), user.DeletionStepEraseMailPreferences) {
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
	store := mailpref.NewMemoryStore()
	other := uuid.New()
	off := false
	for _, person := range []uuid.UUID{f.subjectID, other} {
		if err := store.Apply(context.Background(), person, []mailpref.Change{{Category: mailpref.Master(), Enabled: &off}, {Category: mailpref.Club(mailpref.KindNews), Enabled: &off}}, mailpref.SourceAccountCenter, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	prefs := mailpref.NewService(mailpref.Config{}, store, nil, nil)
	config := f.config
	config.Services = f.group()
	config.ContactConsents = f.consents()
	config.SkyPassWallet = sagaWallet{events: f.events}
	config.MailPreferences = prefs
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
