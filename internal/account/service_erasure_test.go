package account_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Personal data the erasure fixtures carry. None of it may reach an error, a
// stored code, a log line or a metric.
var erasureTestAddresses = []string{"ada.lovelace@std.yildiz.edu.tr", "ada@example.com"}

type erasureService struct {
	mu      sync.Mutex
	server  *httptest.Server
	calls   int
	bodies  []string
	headers []http.Header
	respond func(http.ResponseWriter, *http.Request)
}

func (s *erasureService) set(respond func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = respond
}

func (s *erasureService) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func answerCompleted(counts map[string]int64) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/internal/v1/account-erasures/")
		body, _ := json.Marshal(map[string]any{"request_id": id, "status": "completed", "completed_at": "2026-09-25T12:00:00Z", "counts": counts})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func answerStatus(code int, retryAfter string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(code)
		// A misbehaving service writes personal data back; none may surface.
		_, _ = w.Write([]byte(`{"code":"x","detail":"` + strings.Join(erasureTestAddresses, ",") + `"}`))
	}
}

type fixedAddresses struct {
	emails []string
	err    error
	calls  int
}

func (a *fixedAddresses) ErasureAddresses(context.Context, uuid.UUID) ([]string, error) {
	a.calls++
	return a.emails, a.err
}

// erasureTestStore is what the fixture needs from the memory and the
// PostgreSQL store alike.
type erasureTestStore interface {
	account.Store
	user.Store
	RequestDeletion(context.Context, uuid.UUID, *uuid.UUID) (user.DeletionRequest, error)
	DeletionRequest(context.Context, uuid.UUID) (user.DeletionRequest, error)
	DeletionStepRecords(context.Context, uuid.UUID) ([]user.DeletionStepRecord, error)
	MarkDeletionPlatformBlocked(context.Context, uuid.UUID, time.Time) error
	account.WatchdogStore
}

type erasureFixture struct {
	t             *testing.T
	store         erasureTestStore
	subjectID     uuid.UUID
	request       user.DeletionRequest
	now           time.Time
	services      map[user.DeletionStep]*erasureService
	tokenRequests int
	tokenMu       sync.Mutex
	tokenServer   *httptest.Server
	addresses     *fixedAddresses
	config        account.WorkerConfig
	errors        []string
}

func newErasureFixture(t *testing.T) *erasureFixture {
	t.Helper()
	return newErasureFixtureWith(t, user.NewMemoryStore())
}

func newErasureFixtureWith(t *testing.T, store erasureTestStore) *erasureFixture {
	t.Helper()
	return newErasureFixtureFor(t, store, user.Profile{Email: erasureTestAddresses[1], FirstName: "Ada"})
}

// newErasureFixtureFor starts from core's row of the person as profile holds
// it.
func newErasureFixtureFor(t *testing.T, store erasureTestStore, profile user.Profile) *erasureFixture {
	t.Helper()
	f := &erasureFixture{t: t, store: store, subjectID: uuid.New(), services: map[user.DeletionStep]*erasureService{}}
	ctx := context.Background()
	if _, _, err := user.NewService(f.store).Ensure(ctx, f.subjectID, profile); err != nil {
		t.Fatal(err)
	}
	request, err := f.store.RequestDeletion(ctx, f.subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.request = request
	f.now = request.CreatedAt
	confirmDeletionProjection(t, f.store, request, f.now)
	f.tokenServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.tokenMu.Lock()
		f.tokenRequests++
		f.tokenMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-` + r.FormValue("scope") + `","expires_in":300}`))
	}))
	t.Cleanup(f.tokenServer.Close)
	for _, service := range erasure.Registry() {
		s := &erasureService{respond: answerCompleted(map[string]int64{service.Name + "_rows_erased": 1})}
		s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.calls++
			s.bodies = append(s.bodies, string(body))
			s.headers = append(s.headers, r.Header.Clone())
			respond := s.respond
			s.mu.Unlock()
			respond(w, r)
		}))
		t.Cleanup(s.server.Close)
		f.services[service.Step] = s
	}
	f.addresses = &fixedAddresses{emails: erasureTestAddresses}
	f.config = account.WorkerConfig{
		Now: func() time.Time { return f.now }, Lease: 5 * time.Minute, RetryDelay: 30 * time.Second,
		MaxAttempts: 8, DeferredRetryHorizon: 48 * time.Hour, AccessBlocker: &accountBlockWriter{},
	}
	return f
}

func (f *erasureFixture) serviceErasure(httpClient *http.Client) account.ServiceErasure {
	var steps []account.ServiceStep
	for _, service := range erasure.Registry() {
		steps = append(steps, account.ServiceStep{Step: service.Step, Sender: &erasure.Client{
			Service: service,
			BaseURL: f.services[service.Step].server.URL,
			Tokens: &erasure.ClientCredentials{
				TokenURL: f.tokenServer.URL, ClientID: "core-erasure", Secret: func() (string, error) { return "secret", nil }, Scope: service.Scope,
				Now: func() time.Time { return f.now },
			},
			HTTP: httpClient,
			Now:  func() time.Time { return f.now },
		}})
	}
	return account.ServiceErasure{Steps: steps, Addresses: f.addresses}
}

func (f *erasureFixture) tokens() int {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	return f.tokenRequests
}

func (f *erasureFixture) worker() *account.Worker {
	return account.NewServiceErasureWorker(f.store, f.serviceErasure(nil), f.config)
}

func (f *erasureFixture) run(worker *account.Worker) (bool, error) {
	f.t.Helper()
	worked, err := worker.RunOnce(context.Background())
	if err != nil {
		f.errors = append(f.errors, err.Error())
	}
	return worked, err
}

func (f *erasureFixture) state() user.DeletionRequest {
	f.t.Helper()
	request, err := f.store.DeletionRequest(context.Background(), f.subjectID)
	if err != nil {
		f.t.Fatal(err)
	}
	return request
}

func (f *erasureFixture) records() map[user.DeletionStep]user.DeletionStepRecord {
	f.t.Helper()
	records, err := f.store.DeletionStepRecords(context.Background(), f.request.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[user.DeletionStep]user.DeletionStepRecord{}
	for _, record := range records {
		out[record.Step] = record
	}
	return out
}

func (f *erasureFixture) assertNoPersonalData(texts ...string) {
	f.t.Helper()
	for _, text := range append(append([]string{}, f.errors...), texts...) {
		// The request id is not personal data, and its hex digits can spell
		// "ada".
		lowered := strings.ToLower(strings.ReplaceAll(text, f.request.ID.String(), "<request_id>"))
		for _, value := range append(append([]string{}, erasureTestAddresses...), f.subjectID.String(), "ada") {
			if strings.Contains(lowered, value) {
				f.t.Fatalf("%q carries personal data %q", text, value)
			}
		}
	}
}

func TestServiceErasureCheckpointsEveryServiceWithItsCounts(t *testing.T) {
	t.Parallel()

	f := newErasureFixture(t)
	if worked, err := f.run(f.worker()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted {
		t.Fatalf("request = %+v", state)
	}
	records := f.records()
	for _, service := range erasure.Registry() {
		record, ok := records[service.Step]
		if !ok || !record.CompletedAt.Equal(f.now) || record.Counts[service.Name+"_rows_erased"] != 1 || len(record.Counts) != 1 {
			t.Fatalf("%s proof = %+v ok=%v", service.Step, record, ok)
		}
		s := f.services[service.Step]
		if s.callCount() != 1 {
			t.Fatalf("%s called %d times", service.Step, s.callCount())
		}
		for _, email := range erasureTestAddresses {
			if !strings.Contains(s.bodies[0], email) {
				t.Fatalf("%s body lacks the addresses", service.Step)
			}
		}
	}
	if f.addresses.calls != 1 {
		t.Fatalf("addresses read %d times in one pass", f.addresses.calls)
	}
	f.assertNoPersonalData(f.state().LastErrorCode)
}

func TestServiceErasureTriesEveryServiceWhileOneIsUnavailable(t *testing.T) {
	t.Parallel()

	f := newErasureFixture(t)
	f.services[user.DeletionStepEraseCMS].set(answerStatus(http.StatusServiceUnavailable, "60"))
	worker := f.worker()
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	records := f.records()
	if _, ok := records[user.DeletionStepEraseSkyMail]; !ok {
		t.Fatal("skymail was not checkpointed in the same pass")
	}
	if _, ok := records[user.DeletionStepEraseForms]; !ok {
		t.Fatal("forms was not checkpointed in the same pass")
	}
	if _, ok := records[user.DeletionStepEraseCMS]; ok {
		t.Fatal("cms was checkpointed while unavailable")
	}
	state := f.state()
	if state.Status != user.DeletionRequestPending || state.AttemptCount != 0 || state.LastErrorCode != "erase_cms_failed" ||
		!state.NextAttemptAt.Equal(f.now.Add(time.Minute)) {
		t.Fatalf("deferred request = %+v", state)
	}

	f.now = f.now.Add(time.Minute)
	f.services[user.DeletionStepEraseCMS].set(answerCompleted(map[string]int64{"collection_items_updated": 2}))
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("second pass worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted || state.AttemptCount != 1 {
		t.Fatalf("completed request = %+v", state)
	}
	for step, want := range map[user.DeletionStep]int{
		user.DeletionStepEraseSkyMail: 1, user.DeletionStepEraseCMS: 2, user.DeletionStepEraseForms: 1,
	} {
		if got := f.services[step].callCount(); got != want {
			t.Fatalf("%s calls = %d, want %d: a completed step ran again", step, got, want)
		}
	}
	if cms := f.records()[user.DeletionStepEraseCMS]; cms.Counts["collection_items_updated"] != 2 {
		t.Fatalf("cms proof = %+v", cms)
	}
	f.assertNoPersonalData()
}

func TestServiceErasureDefersInProgressAndBusyAnswersByTheClampedRetryAfter(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		code       int
		retryAfter string
		wait       time.Duration
	}{
		{code: http.StatusAccepted, retryAfter: "5", wait: 30 * time.Second},
		{code: http.StatusAccepted, retryAfter: "", wait: 5 * time.Minute},
		{code: http.StatusServiceUnavailable, retryAfter: "7200", wait: 15 * time.Minute},
		{code: http.StatusTooManyRequests, retryAfter: "90", wait: 90 * time.Second},
	} {
		f := newErasureFixture(t)
		f.services[user.DeletionStepEraseSkyMail].set(answerStatus(tc.code, tc.retryAfter))
		if worked, err := f.run(f.worker()); !worked || err == nil {
			t.Fatalf("%d: worked=%v err=%v", tc.code, worked, err)
		}
		state := f.state()
		if state.Status != user.DeletionRequestPending || state.AttemptCount != 0 || !state.NextAttemptAt.Equal(f.now.Add(tc.wait)) {
			t.Fatalf("%d Retry-After %q: request = %+v, want wait %s", tc.code, tc.retryAfter, state, tc.wait)
		}
		f.assertNoPersonalData(state.LastErrorCode)
	}
}

func TestServiceErasureTimeoutIsDeferred(t *testing.T) {
	t.Parallel()

	f := newErasureFixture(t)
	release := make(chan struct{})
	defer close(release)
	f.services[user.DeletionStepEraseForms].set(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	// Only the stalled service's call times out; the others keep the
	// production timeout, so a slow machine cannot make them fail too.
	services := f.serviceErasure(nil)
	services.Steps[2].Sender.(*erasure.Client).HTTP = &http.Client{Timeout: 200 * time.Millisecond}
	if worked, err := f.run(account.NewServiceErasureWorker(f.store, services, f.config)); !worked || err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	state := f.state()
	if state.Status != user.DeletionRequestPending || state.AttemptCount != 0 || !state.NextAttemptAt.Equal(f.now.Add(5*time.Minute)) {
		t.Fatalf("timed-out request = %+v", state)
	}
	if len(f.records()) != 2 {
		t.Fatalf("the other services were not checkpointed: %+v", f.records())
	}
	f.assertNoPersonalData(state.LastErrorCode)
}

func TestServiceErasureSpendsTheOrdinaryBudgetOnceTheHorizonHasPassed(t *testing.T) {
	t.Parallel()

	f := newErasureFixture(t)
	f.config.MaxAttempts = 2
	f.now = f.request.CreatedAt.Add(49 * time.Hour)
	f.services[user.DeletionStepEraseCMS].set(answerStatus(http.StatusServiceUnavailable, "60"))
	worker := f.worker()
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	state := f.state()
	if state.Status != user.DeletionRequestPending || state.AttemptCount != 1 || !state.NextAttemptAt.Equal(f.now.Add(30*time.Second)) {
		t.Fatalf("after horizon, first failure = %+v", state)
	}
	f.now = state.NextAttemptAt
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestManualIntervention || state.LastErrorCode != "erase_cms_failed" || state.AttemptCount != 2 {
		t.Fatalf("budget exhausted = %+v", state)
	}
	f.assertNoPersonalData()
}

func TestServiceErasureUnauthorizedDropsTheTokenAndSpendsAnAttempt(t *testing.T) {
	t.Parallel()

	f := newErasureFixture(t)
	f.services[user.DeletionStepEraseForms].set(answerStatus(http.StatusUnauthorized, "600"))
	worker := f.worker()
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	state := f.state()
	if state.Status != user.DeletionRequestPending || state.AttemptCount != 1 || state.LastErrorCode != "erase_forms_failed" ||
		!state.NextAttemptAt.Equal(f.now.Add(30*time.Second)) {
		t.Fatalf("after 401 = %+v", state)
	}
	if f.tokens() != 3 {
		t.Fatalf("token requests after first pass = %d", f.tokens())
	}

	f.now = state.NextAttemptAt
	f.services[user.DeletionStepEraseForms].set(answerCompleted(map[string]int64{"responses_redacted": 4}))
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if f.tokens() != 4 {
		t.Fatalf("the rejected token was reused: token requests = %d", f.tokens())
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted {
		t.Fatalf("request = %+v", state)
	}
	f.assertNoPersonalData()
}

func TestServiceErasurePermanentRejectionGoesStraightToManualIntervention(t *testing.T) {
	t.Parallel()

	for _, code := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict} {
		f := newErasureFixture(t)
		f.services[user.DeletionStepEraseCMS].set(answerStatus(code, ""))
		f.services[user.DeletionStepEraseSkyMail].set(answerStatus(http.StatusServiceUnavailable, "60"))
		if worked, err := f.run(f.worker()); !worked || err == nil {
			t.Fatalf("%d: worked=%v err=%v", code, worked, err)
		}
		state := f.state()
		want := "erase_cms_rejected_" + strconv.Itoa(code)
		if state.Status != user.DeletionRequestManualIntervention || state.LastErrorCode != want || state.AttemptCount != 1 {
			t.Fatalf("%d: request = %+v, want manual with %s", code, state, want)
		}
		if _, ok := f.records()[user.DeletionStepEraseForms]; !ok {
			t.Fatalf("%d: forms was not attempted beside the rejection", code)
		}
		f.assertNoPersonalData(state.LastErrorCode)
	}
}

func TestServiceErasureRetryStampsTheChangeTimeNotTheNextAttempt(t *testing.T) {
	t.Parallel()
	testRetryStampsTheChangeTimeNotTheNextAttempt(t, user.NewMemoryStore())
}

// testRetryStampsTheChangeTimeNotTheNextAttempt: a deferral, an ordinary
// failure and manual intervention each stamp updated_at with the moment of the
// change; the next attempt's time goes only to next_attempt_at. Account
// Center orders the statuses it reads by updatedAt, so a stamp in the future
// kept it on manual_intervention after a completed retry (ticket 13).
func testRetryStampsTheChangeTimeNotTheNextAttempt(t *testing.T, store erasureTestStore) {
	t.Helper()
	f := newErasureFixtureWith(t, store)
	worker := f.worker()
	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter, *http.Request)
		status user.DeletionRequestStatus
		wait   time.Duration
	}{
		{name: "deferred", answer: answerStatus(http.StatusServiceUnavailable, "600"), status: user.DeletionRequestPending, wait: 10 * time.Minute},
		{name: "ordinary", answer: answerStatus(http.StatusUnauthorized, ""), status: user.DeletionRequestPending, wait: 30 * time.Second},
		{name: "manual", answer: answerStatus(http.StatusForbidden, ""), status: user.DeletionRequestManualIntervention, wait: 30 * time.Second},
	} {
		f.services[user.DeletionStepEraseCMS].set(tc.answer)
		if worked, err := f.run(worker); !worked || err == nil {
			t.Fatalf("%s: worked=%v err=%v", tc.name, worked, err)
		}
		state := f.state()
		if state.Status != tc.status || !state.NextAttemptAt.Equal(f.now.Add(tc.wait)) {
			t.Fatalf("%s: request = %+v, want %s with the next attempt in %s", tc.name, state, tc.status, tc.wait)
		}
		if !state.UpdatedAt.Equal(f.now) {
			t.Fatalf("%s: updated_at = %s, want the moment of the change %s (next attempt %s)", tc.name, state.UpdatedAt, f.now, state.NextAttemptAt)
		}
		f.now = state.NextAttemptAt
	}
	f.assertNoPersonalData()
}

// main.go logs every error the worker returns as one line. Spec §2.7: the
// line carries the request_id, the step and the code, never the subject, an
// address or a name.
func TestWorkerErrorLinesCarryTheRequestIDAndNoSubject(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(*erasureFixture)
		code  string
	}{
		{name: "marker write failed", code: "platform_block_failed", setup: func(f *erasureFixture) {
			f.config.AccessBlocker = &accountBlockWriter{err: errors.New("redis unavailable")}
		}},
		{name: "addresses unreadable", code: "erasure_addresses_failed", setup: func(f *erasureFixture) {
			f.addresses.err = errors.New("identity directory unavailable")
		}},
		{name: "service deferred", code: "erase_cms_failed", setup: func(f *erasureFixture) {
			f.services[user.DeletionStepEraseCMS].set(answerStatus(http.StatusServiceUnavailable, "60"))
		}},
		{name: "token refused", code: "erase_cms_failed", setup: func(f *erasureFixture) {
			f.services[user.DeletionStepEraseCMS].set(answerStatus(http.StatusUnauthorized, ""))
		}},
		{name: "rejected", code: "erase_cms_rejected_403", setup: func(f *erasureFixture) {
			f.services[user.DeletionStepEraseCMS].set(answerStatus(http.StatusForbidden, ""))
		}},
	} {
		f := newErasureFixture(t)
		tc.setup(f)
		_, err := f.run(f.worker())
		if err == nil {
			t.Fatalf("%s: the pass succeeded", tc.name)
		}
		line := fmt.Sprintf("account erasure worker: %v", err)
		if want := "account erasure " + tc.code + " request_id=" + f.request.ID.String() + ":"; !strings.Contains(line, want) {
			t.Fatalf("%s: line %q lacks %q", tc.name, line, want)
		}
		if state := f.state(); state.LastErrorCode != tc.code {
			t.Fatalf("%s: stored code %q, line code %q", tc.name, state.LastErrorCode, tc.code)
		}
		f.assertNoPersonalData(line)
	}
}

func TestServiceErasureStopsAtALostLease(t *testing.T) {
	t.Parallel()

	f := newErasureFixture(t)
	thieves := make(chan user.DeletionRequest, 1)
	f.services[user.DeletionStepEraseSkyMail].set(func(w http.ResponseWriter, r *http.Request) {
		// The worker stalls past its lease; another worker claims the request.
		claimed, ok, err := f.store.ClaimDeletionRequest(context.Background(), f.now.Add(10*time.Minute), time.Minute)
		if err != nil || !ok {
			t.Errorf("thief claim ok=%v err=%v", ok, err)
		}
		thieves <- claimed
		answerCompleted(map[string]int64{"recipients_deleted": 1})(w, r)
	})
	if worked, err := f.run(f.worker()); !worked || !errors.Is(err, user.ErrLeaseLost) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if records := f.records(); len(records) != 0 {
		t.Fatalf("a fenced-out worker wrote proof: %+v", records)
	}
	if f.services[user.DeletionStepEraseCMS].callCount() != 0 || f.services[user.DeletionStepEraseForms].callCount() != 0 {
		t.Fatal("the fenced-out worker went on to call other services")
	}
	thief := <-thieves
	state := f.state()
	if state.Status != user.DeletionRequestProcessing || state.LeaseToken == nil || thief.LeaseToken == nil || *state.LeaseToken != *thief.LeaseToken {
		t.Fatalf("the new lease was disturbed: %+v", state)
	}
}

func TestServiceErasureAddressFailureCallsNoService(t *testing.T) {
	t.Parallel()

	f := newErasureFixture(t)
	f.addresses.err = errors.New("identity directory unavailable")
	if worked, err := f.run(f.worker()); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	for step, service := range f.services {
		if service.callCount() != 0 {
			t.Fatalf("%s called without addresses", step)
		}
	}
	if state := f.state(); state.Status != user.DeletionRequestPending || state.LastErrorCode != "erasure_addresses_failed" || state.AttemptCount != 1 {
		t.Fatalf("request = %+v", state)
	}
}

func TestNewServiceErasureFollowsTheRegistry(t *testing.T) {
	t.Parallel()

	secretReads := 0
	config := erasure.Config{ClientID: "core-erasure", ClientSecret: func() (string, error) {
		secretReads++
		return "secret", nil
	}}
	for _, service := range erasure.Registry() {
		config.Endpoints = append(config.Endpoints, erasure.Endpoint{Service: service, BaseURL: "http://" + service.Name + ":8080"})
	}
	addresses := &fixedAddresses{}
	services := account.NewServiceErasure(config, "http://keycloak:8080/realms/e-skylab/protocol/openid-connect/token", addresses)
	if services.Addresses != addresses || len(services.Steps) != 3 {
		t.Fatalf("service erasure = %+v", services)
	}
	for i, service := range erasure.Registry() {
		client, ok := services.Steps[i].Sender.(*erasure.Client)
		if !ok || services.Steps[i].Step != service.Step || client.BaseURL != "http://"+service.Name+":8080" || client.Service != service {
			t.Fatalf("step %d = %+v", i, services.Steps[i])
		}
		tokens, ok := client.Tokens.(*erasure.ClientCredentials)
		if !ok || tokens.Scope != service.Scope || tokens.ClientID != "core-erasure" ||
			tokens.TokenURL != "http://keycloak:8080/realms/e-skylab/protocol/openid-connect/token" {
			t.Fatalf("step %d tokens = %+v", i, client.Tokens)
		}
		// Each token cache reads the secret from the configuration when it
		// asks for a token, never at construction.
		if reads := secretReads; tokens.Secret == nil || reads != i {
			t.Fatalf("step %d: secret source=%v, read %d times before use", i, tokens.Secret != nil, reads)
		}
		if secret, err := tokens.Secret(); err != nil || secret != "secret" || secretReads != i+1 {
			t.Fatalf("step %d secret source does not read the configuration", i)
		}
	}
	if services.Steps[0].Sender.(*erasure.Client).Tokens == services.Steps[1].Sender.(*erasure.Client).Tokens {
		t.Fatal("services share one token cache; a token must carry only its own service's role")
	}
}
