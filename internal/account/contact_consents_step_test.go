package account_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// consentsErased is a contact consent eraser with nothing to erase.
type consentsErased struct{}

func (consentsErased) EraseSubject(context.Context, uuid.UUID, []string) (int64, error) {
	return 0, nil
}

// sagaConsents records the erase_contact_consents step in the saga's order
// and the addresses it was given.
type sagaConsents struct {
	events *sagaEvents
	mu     *sync.Mutex
	got    *[][]string
	err    error
}

func (c sagaConsents) EraseSubject(_ context.Context, _ uuid.UUID, addresses []string) (int64, error) {
	c.events.add("erase_contact_consents")
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.got = append(*c.got, append([]string(nil), addresses...))
	return int64(len(addresses)), c.err
}

func (f *sagaFixture) consents() sagaConsents {
	if f.consentCalls == nil {
		f.consentCalls = &consentCalls{}
	}
	return sagaConsents{events: f.events, mu: &f.consentCalls.mu, got: &f.consentCalls.addresses, err: f.consentCalls.err}
}

type consentCalls struct {
	mu        sync.Mutex
	addresses [][]string
	err       error
}

// The consents go first after the logout, before any service: a person who
// asked for erasure gets no more invitations while a service is late (the CMS
// waits out its token window, a service may be down for days).
func TestErasureSagaErasesContactConsentsWithThePersonsAddressesBeforeAnyService(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	if worked, err := f.run(f.saga()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	events := f.events.list()
	consents := slices.Index(events, "erase_contact_consents")
	if consents < 0 || consents < slices.Index(events, "logout_sessions") {
		t.Fatalf("saga order = %v", events)
	}
	for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core"} {
		if at := slices.Index(events, later); at < 0 || at < consents {
			t.Fatalf("%s before erase_contact_consents: %v", later, events)
		}
	}
	f.consentCalls.mu.Lock()
	got := f.consentCalls.addresses
	f.consentCalls.mu.Unlock()
	if len(got) != 1 || !slices.Contains(got[0], sagaPersonal) || !slices.Contains(got[0], sagaSchool) {
		t.Fatalf("consent eraser got %v", got)
	}
	if !slices.Contains(f.checkpoints(), user.DeletionStepEraseContactConsents) {
		t.Fatalf("no checkpoint for erase_contact_consents: %v", f.checkpoints())
	}
}

func TestErasureSagaStopsWhenContactConsentsCannotBeErased(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	f.consentCalls = &consentCalls{err: errors.New("database unavailable")}
	worked, err := f.run(f.saga())
	if !worked || err == nil || !strings.Contains(err.Error(), "erase_contact_consents_failed") {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	for _, later := range []string{"erase_skymail", "erase_cms", "erase_forms", "anonymize_core"} {
		if f.events.count(later) != 0 {
			t.Fatalf("the saga went on to %s past a failed consent erasure: %v", later, f.events.list())
		}
	}
	if state := f.state(); state.Status != user.DeletionRequestPending || state.LastErrorCode != "erase_contact_consents_failed" {
		t.Fatalf("request = %+v", state)
	}

	// Without an eraser the step fails too: a person is never reported
	// erased while their consents may remain.
	g := newSagaFixture(t, user.NewMemoryStore())
	config := g.config
	config.Services = g.group()
	config.ContactConsents = nil
	worker := newWorkerWithoutConsents(g, config)
	if _, err := g.run(worker); err == nil || !strings.Contains(err.Error(), "contact consent eraser unavailable") {
		t.Fatalf("nil eraser: %v", err)
	}
}

func newWorkerWithoutConsents(f *sagaFixture, config account.WorkerConfig) *account.Worker {
	return account.NewWorkerWithConfiguredWaits(sagaStore{erasureTestStore: f.store, events: f.events}, f.identity, config, sagaMedia{events: f.events})
}
