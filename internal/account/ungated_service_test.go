package account_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// withRegistryWaits gives each step the wait its registry entry has, as
// NewServiceErasure does: the CMS (inscribed) has no access gate.
func withRegistryWaits(services account.ServiceErasure) account.ServiceErasure {
	for i, step := range services.Steps {
		for _, entry := range erasure.Registry() {
			if entry.Step == step.Step {
				services.Steps[i].WaitAfterIdentityClosed = entry.WaitAfterIdentityClosed
			}
		}
	}
	return services
}

func TestErasureSagaWaitsForTheTokenWindowBeforeCallingAnUngatedService(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	start := f.now
	// The group carries no wait of its own: the registry's floor applies.
	worker := f.sagaAsProduction(f.group())
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if got := f.services[user.DeletionStepEraseCMS].callCount(); got != 0 {
		t.Fatalf("cms called %d times inside the token window", got)
	}
	want := []user.DeletionStep{user.DeletionStepDisableIdentity, user.DeletionStepLogoutSessions, user.DeletionStepEraseSkyMail, user.DeletionStepEraseForms}
	if got := f.checkpoints(); !slices.Equal(got, want) {
		t.Fatalf("checkpoints = %v, want %v", got, want)
	}
	state := f.state()
	if state.Status != user.DeletionRequestPending || state.AttemptCount != 0 || state.LastErrorCode != "erase_cms_waiting" ||
		!state.NextAttemptAt.Equal(start.Add(6*time.Minute)) {
		t.Fatalf("waiting request = %+v", state)
	}
	if f.events.count("anonymize_core") != 0 || f.events.count("delete_identity") != 0 {
		t.Fatalf("a step after the services ran: %v", f.events.list())
	}

	f.now = state.NextAttemptAt
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted || state.AttemptCount != 1 {
		t.Fatalf("request = %+v", state)
	}
	if got := f.checkpoints(); !slices.Equal(got, sagaSteps) {
		t.Fatalf("checkpoints = %v", got)
	}
	if cms := f.records()[user.DeletionStepEraseCMS]; !cms.CompletedAt.Equal(start.Add(6 * time.Minute)) {
		t.Fatalf("cms proof = %+v", cms)
	}
	for event, want := range map[string]int{"erase_skymail": 1, "erase_cms": 1, "erase_forms": 1, "disable_identity": 1, "logout_sessions": 1} {
		if got := f.events.count(event); got != want {
			t.Fatalf("%s ran %d times, want %d: %v", event, got, want, f.events.list())
		}
	}
	f.assertNoPersonalData(state.LastErrorCode)
}

func TestErasureSagaCountsTheTokenWindowFromTheIdentityCheckpoints(t *testing.T) {
	t.Parallel()
	testTokenWindowCountsFromTheIdentityCheckpoints(t, user.NewMemoryStore())
}

// testTokenWindowCountsFromTheIdentityCheckpoints: Keycloak does not read the
// access marker, so a session can mint tokens until disable_identity. The
// wait runs from the later identity checkpoint, not from the block, also when
// those checkpoints were written in an earlier pass.
func testTokenWindowCountsFromTheIdentityCheckpoints(t *testing.T, store erasureTestStore) {
	t.Helper()
	f := newSagaFixture(t, store)
	blocked := f.now
	// The worker reaches the request ten minutes after the block, and
	// SkyMail asks for a minute.
	f.now = blocked.Add(10 * time.Minute)
	closed := f.now
	f.answer(user.DeletionStepEraseSkyMail, answerStatus(http.StatusServiceUnavailable, "60"))
	worker := f.sagaWith(withRegistryWaits(f.group()))
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	state := f.state()
	if state.LastErrorCode != "erase_skymail_failed" || state.AttemptCount != 0 || !state.NextAttemptAt.Equal(closed.Add(time.Minute)) {
		t.Fatalf("first pass request = %+v", state)
	}

	// The next pass reads the identity checkpoints from the store: the wait
	// still ends six minutes after the disable, not after the block.
	f.now = state.NextAttemptAt
	f.answer(user.DeletionStepEraseSkyMail, answerCompleted(map[string]int64{"skymail_rows_erased": 1}))
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	state = f.state()
	if state.LastErrorCode != "erase_cms_waiting" || state.AttemptCount != 0 || !state.NextAttemptAt.Equal(closed.Add(6*time.Minute)) {
		t.Fatalf("second pass request = %+v", state)
	}
	if got := f.services[user.DeletionStepEraseCMS].callCount(); got != 0 {
		t.Fatalf("cms called %d times inside the token window", got)
	}

	f.now = state.NextAttemptAt
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted {
		t.Fatalf("request = %+v", state)
	}
	f.assertNoPersonalData()
}

func TestServiceErasureAFailureNamesTheFailedServiceNotTheWaitingOne(t *testing.T) {
	t.Parallel()

	f := newErasureFixture(t)
	f.services[user.DeletionStepEraseForms].set(answerStatus(http.StatusNotFound, ""))
	worker := account.NewServiceErasureWorker(f.store, withRegistryWaits(f.serviceErasure(nil)), f.config)
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestManualIntervention || state.LastErrorCode != "erase_forms_rejected_404" {
		t.Fatalf("request = %+v", state)
	}
	if got := f.services[user.DeletionStepEraseCMS].callCount(); got != 0 {
		t.Fatalf("cms called %d times inside the token window", got)
	}
	f.assertNoPersonalData()
}

// Past the deferral horizon a pure wait is still refunded and comes back when
// the window ends: it is bounded by the window, not by a service's answer.
func TestErasureSagaWaitPastTheHorizonSpendsNoAttempt(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	// The worker reaches the request 72 hours after it was made (the
	// horizon is 48), so the identity closes now.
	f.now = f.now.Add(72 * time.Hour)
	closed := f.now
	worker := f.sagaAsProduction(f.group())
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	// Spending attempts here sent the request to manual intervention after
	// eight 30-second retries, all inside the window.
	state := f.state()
	if state.Status != user.DeletionRequestPending || state.AttemptCount != 0 || state.LastErrorCode != "erase_cms_waiting" ||
		!state.NextAttemptAt.Equal(closed.Add(6*time.Minute)) {
		t.Fatalf("waiting request = %+v", state)
	}
	f.now = state.NextAttemptAt
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted || state.AttemptCount != 1 {
		t.Fatalf("request = %+v", state)
	}
	if got := f.services[user.DeletionStepEraseCMS].callCount(); got != 1 {
		t.Fatalf("cms called %d times", got)
	}
}

func TestIdentityClosedAtCapsAFutureTimeAndTakesNowForNone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	blocked := now.Add(-time.Minute)
	disabled := now.Add(-30 * time.Second)

	cases := []struct {
		name        string
		request     user.DeletionRequest
		checkpoints map[user.DeletionStep]time.Time
		want        time.Time
	}{
		{"latest of block and identity checkpoints", user.DeletionRequest{PlatformBlockedAt: &blocked},
			map[user.DeletionStep]time.Time{user.DeletionStepDisableIdentity: disabled}, disabled},
		{"nothing known is now", user.DeletionRequest{}, nil, now},
		{"a future block is capped", user.DeletionRequest{PlatformBlockedAt: &future}, nil, now.Add(account.ClosedAtTolerance)},
		{"a future checkpoint is capped", user.DeletionRequest{PlatformBlockedAt: &blocked},
			map[user.DeletionStep]time.Time{user.DeletionStepLogoutSessions: future}, now.Add(account.ClosedAtTolerance)},
	}
	for _, tc := range cases {
		if got := account.IdentityClosedAt(tc.request, tc.checkpoints, now); !got.Equal(tc.want) {
			t.Fatalf("%s: closed at %s, want %s", tc.name, got, tc.want)
		}
	}
	if !strings.HasPrefix(account.ClosedAtTolerance.String(), "1m") {
		t.Fatalf("tolerance = %s", account.ClosedAtTolerance)
	}
}
