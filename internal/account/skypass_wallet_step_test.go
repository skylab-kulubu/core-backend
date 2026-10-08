package account_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// walletErased is a SkyPass wallet eraser with nothing to withdraw.
type walletErased struct{}

func (walletErased) EraseSubject(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}

// sagaWallet records the erase_skypass_wallet step in the saga's order.
type sagaWallet struct {
	events *sagaEvents
	err    error
}

func (w sagaWallet) EraseSubject(context.Context, uuid.UUID) (int64, error) {
	w.events.add("erase_skypass_wallet")
	return 1, w.err
}

// Right after the consents and before any service: the pass stops opening
// the door, and Google forgets the name on it, while a service may still
// hold the saga for days.
func TestErasureSagaWithdrawsTheWalletPassBeforeAnyService(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	if worked, err := f.run(f.saga()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	events := f.events.list()
	wallet := slices.Index(events, "erase_skypass_wallet")
	consents := slices.Index(events, "erase_contact_consents")
	if wallet < 0 || consents < 0 || wallet < consents {
		t.Fatalf("events = %v", events)
	}
	for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core", "delete_identity"} {
		if index := slices.Index(events, later); index >= 0 && index < wallet {
			t.Fatalf("%s before erase_skypass_wallet: %v", later, events)
		}
	}
	if !slices.Contains(f.checkpoints(), user.DeletionStepEraseSkyPassWallet) {
		t.Fatalf("no checkpoint for erase_skypass_wallet: %v", f.checkpoints())
	}
}

// deferredWallet is Google down: the wallet eraser's deferred failure.
type deferredWallet struct{ at time.Time }

func (d deferredWallet) Error() string {
	return "skypass: google wallet is unavailable: 1 of 1 passes not withdrawn"
}
func (d deferredWallet) RetryAt() time.Time { return d.at }

func TestErasureSagaStopsWhenTheWalletPassCannotBeWithdrawn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		eraser   func(*sagaFixture) any
		attempts int
	}{
		{name: "refused", attempts: 1, eraser: func(f *sagaFixture) any {
			return sagaWallet{events: f.events, err: errors.New("skypass: google wallet is off: 1 SkyPass wallet passes wait to be withdrawn from Google")}
		}},
		{name: "google down", attempts: 0, eraser: func(f *sagaFixture) any {
			return sagaWallet{events: f.events, err: deferredWallet{at: f.now.Add(5 * time.Minute)}}
		}},
		{name: "no eraser", attempts: 1, eraser: func(*sagaFixture) any { return nil }},
	} {
		f := newSagaFixture(t, user.NewMemoryStore())
		config := f.config
		config.Services = f.group()
		config.ContactConsents = f.consents()
		switch eraser := tc.eraser(f).(type) {
		case sagaWallet:
			config.SkyPassWallet = eraser
		default:
			config.SkyPassWallet = nil
		}
		worked, err := f.run(newWorkerWithoutConsents(f, config))
		if !worked || err == nil || !strings.Contains(err.Error(), "erase_skypass_wallet_failed") {
			t.Fatalf("%s: worked=%v err=%v", tc.name, worked, err)
		}
		for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core", "delete_identity"} {
			if f.events.count(later) != 0 {
				t.Fatalf("%s: the saga went on to %s: %v", tc.name, later, f.events.list())
			}
		}
		state := f.state()
		if state.Status != user.DeletionRequestPending || state.LastErrorCode != "erase_skypass_wallet_failed" || state.AttemptCount != tc.attempts {
			t.Fatalf("%s: request = %+v", tc.name, state)
		}
		if slices.Contains(f.checkpoints(), user.DeletionStepEraseSkyPassWallet) {
			t.Fatalf("%s: checkpointed a failed withdrawal", tc.name)
		}
		f.assertNoPersonalData(state.LastErrorCode)
	}
}
