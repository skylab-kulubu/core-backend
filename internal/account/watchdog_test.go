package account_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type openRequests struct {
	mu       sync.Mutex
	requests []user.DeletionRequest
	err      error
}

func (o *openRequests) OpenDeletionRequests(context.Context) ([]user.DeletionRequest, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]user.DeletionRequest(nil), o.requests...), o.err
}

type attentionLog struct {
	mu     sync.Mutex
	events []account.Attention
}

func (a *attentionLog) record(event account.Attention) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
}

func (a *attentionLog) take() []account.Attention {
	a.mu.Lock()
	defer a.mu.Unlock()
	events := a.events
	a.events = nil
	return events
}

func openRequest(status user.DeletionRequestStatus, age time.Duration, now time.Time, code string) user.DeletionRequest {
	return user.DeletionRequest{
		ID: uuid.New(), SubjectID: uuid.New(), Status: status, CreatedAt: now.Add(-age), LastErrorCode: code,
	}
}

func TestWatchdogGaugesCountOpenOverdueAndManualRequests(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	fresh := openRequest(user.DeletionRequestPending, 24*time.Hour, now, "")
	overdue := openRequest(user.DeletionRequestProcessing, 21*24*time.Hour, now, "erase_cms_failed")
	manual := openRequest(user.DeletionRequestManualIntervention, 2*24*time.Hour, now, "erase_forms_rejected_403")
	store := &openRequests{requests: []user.DeletionRequest{fresh, overdue, manual}}
	gauges := account.NewErasureGauges()
	if got := gauges.Prometheus(); got != "" {
		t.Fatalf("gauges rendered before the first count: %q", got)
	}
	events := &attentionLog{}
	watchdog := account.NewWatchdog(store, gauges, account.WatchdogConfig{
		AlertAfter: 480 * time.Hour, Now: func() time.Time { return now }, Attention: events.record,
	})

	if err := watchdog.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"# TYPE skylab_account_erasure_open_requests gauge",
		"skylab_account_erasure_open_requests 3",
		"# TYPE skylab_account_erasure_overdue_requests gauge",
		"skylab_account_erasure_overdue_requests 1",
		"# TYPE skylab_account_erasure_manual_intervention_requests gauge",
		"skylab_account_erasure_manual_intervention_requests 1",
		"# TYPE skylab_account_erasure_oldest_open_age_seconds gauge",
		"skylab_account_erasure_oldest_open_age_seconds 1814400",
		"",
	}, "\n")
	if got := gauges.Prometheus(); got != want {
		t.Fatalf("gauges =\n%s\nwant\n%s", got, want)
	}
	got := events.take()
	if len(got) != 2 {
		t.Fatalf("attention events = %+v", got)
	}
	byRequest := map[uuid.UUID]account.Attention{}
	for _, event := range got {
		byRequest[event.RequestID] = event
	}
	if event := byRequest[overdue.ID]; event.Reason != "overdue" || event.Step != user.DeletionStepEraseCMS || event.Code != "erase_cms_failed" {
		t.Fatalf("overdue event = %+v", event)
	}
	if event := byRequest[manual.ID]; event.Reason != "manual_intervention" || event.Step != user.DeletionStepEraseForms || event.Code != "erase_forms_rejected_403" {
		t.Fatalf("manual event = %+v", event)
	}

	// Each state is announced once while it lasts.
	if err := watchdog.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again := events.take(); len(again) != 0 {
		t.Fatalf("repeated attention events = %+v", again)
	}

	// The fresh request crosses day 20; the manual one is retried and leaves.
	now = now.Add(20 * 24 * time.Hour)
	store.mu.Lock()
	store.requests = []user.DeletionRequest{fresh, overdue}
	store.mu.Unlock()
	if err := watchdog.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	crossed := events.take()
	if len(crossed) != 1 || crossed[0].RequestID != fresh.ID || crossed[0].Reason != "overdue" || crossed[0].Step != "" || crossed[0].Code != "" {
		t.Fatalf("threshold crossing events = %+v", crossed)
	}
	if got := gauges.Prometheus(); !strings.Contains(got, "skylab_account_erasure_open_requests 2\n") ||
		!strings.Contains(got, "skylab_account_erasure_overdue_requests 2\n") ||
		!strings.Contains(got, "skylab_account_erasure_manual_intervention_requests 0\n") {
		t.Fatalf("gauges after crossing =\n%s", got)
	}

	// The manual request comes back to manual intervention: announced again.
	store.mu.Lock()
	store.requests = append(store.requests, manual)
	store.mu.Unlock()
	if err := watchdog.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if back := events.take(); len(back) != 2 {
		// manual again, and now also overdue (22 days old)
		t.Fatalf("returning manual request events = %+v", back)
	}
}

func TestWatchdogKeepsTheLastCountWhenTheStoreFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	store := &openRequests{err: errors.New("database unavailable")}
	gauges := account.NewErasureGauges()
	watchdog := account.NewWatchdog(store, gauges, account.WatchdogConfig{AlertAfter: 480 * time.Hour, Now: func() time.Time { return now }})
	if err := watchdog.RunOnce(context.Background()); err == nil {
		t.Fatal("store failure hidden")
	}
	if got := gauges.Prometheus(); got != "" {
		t.Fatalf("a failed first count published zeros: %q", got)
	}
	store.err = nil
	if err := watchdog.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := gauges.Prometheus(); !strings.Contains(got, "skylab_account_erasure_open_requests 0\n") ||
		!strings.Contains(got, "skylab_account_erasure_oldest_open_age_seconds 0\n") {
		t.Fatalf("empty gauges = %q", got)
	}
}

func TestAttentionLineCarriesOnlyTheRequestReasonStepAndCode(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	logger := log.New(&out, "", 0)
	requestID := uuid.MustParse("0b6f2a3e-1111-4222-8333-944444444444")
	account.LogAttention(logger)(account.Attention{
		RequestID: requestID, Reason: "manual_intervention", Step: user.DeletionStepEraseCMS, Code: "erase_cms_rejected_403",
	})
	account.LogAttention(logger)(account.Attention{RequestID: requestID, Reason: "overdue"})
	want := "account_erasure_attention request_id=0b6f2a3e-1111-4222-8333-944444444444 reason=manual_intervention step=erase_cms code=erase_cms_rejected_403\n" +
		"account_erasure_attention request_id=0b6f2a3e-1111-4222-8333-944444444444 reason=overdue step=- code=-\n"
	if out.String() != want {
		t.Fatalf("attention lines =\n%s\nwant\n%s", out.String(), want)
	}
}

// Not parallel: it captures the process-wide logger, and parallel tests in
// this package run only after it has restored it.
func TestErasurePathsLeaveNoPersonalDataInErrorsLogsOrMetrics(t *testing.T) {
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	defer log.SetOutput(previous)

	f := newErasureFixture(t)
	f.config.MaxAttempts = 2
	worker := f.worker()
	pass := func(step user.DeletionStep, respond func(w http.ResponseWriter, r *http.Request)) {
		t.Helper()
		f.services[step].set(respond)
		_, _ = f.run(worker)
		f.now = f.state().NextAttemptAt
	}
	pass(user.DeletionStepEraseSkyMail, answerStatus(http.StatusServiceUnavailable, "60"))
	pass(user.DeletionStepEraseSkyMail, answerStatus(http.StatusUnauthorized, ""))
	pass(user.DeletionStepEraseSkyMail, answerStatus(http.StatusForbidden, ""))
	if state := f.state(); state.Status != user.DeletionRequestManualIntervention {
		t.Fatalf("rejection did not reach manual intervention: %+v", state)
	}

	g := newErasureFixture(t)
	g.services[user.DeletionStepEraseForms].set(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"request_id":"` + g.request.ID.String() + `","status":"completed","counts":{"` + erasureTestAddresses[1] + `":1}}`))
	})
	_, _ = g.run(g.worker())
	g.addresses.err = errors.New("directory unavailable")
	g.now = g.state().NextAttemptAt
	_, _ = g.run(g.worker())

	gauges := account.NewErasureGauges()
	logger := log.New(&captured, "", 0)
	now := f.now.Add(30 * 24 * time.Hour)
	watchdog := account.NewWatchdog(f.store, gauges, account.WatchdogConfig{
		AlertAfter: 480 * time.Hour, Now: func() time.Time { return now }, Attention: account.LogAttention(logger),
	})
	if err := watchdog.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(captured.String(), "account_erasure_attention") {
		t.Fatalf("no attention line was written: %q", captured.String())
	}

	var proof []string
	for _, fixture := range []*erasureFixture{f, g} {
		for step, record := range fixture.records() {
			proof = append(proof, string(step))
			for key := range record.Counts {
				proof = append(proof, key)
			}
		}
		proof = append(proof, fixture.state().LastErrorCode)
	}
	texts := append([]string{captured.String(), gauges.Prometheus()}, proof...)
	texts = append(texts, g.errors...)
	f.assertNoPersonalData(texts...)
	g.assertNoPersonalData(texts...)
	if len(f.errors) < 2 || len(g.errors) < 2 {
		t.Fatalf("the failure paths did not run: %v %v", f.errors, g.errors)
	}
}
