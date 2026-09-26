package account_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The whole saga (spec §4): disable_identity → logout_sessions →
// erase_skymail, erase_cms, erase_forms → anonymize_core →
// erase_profile_media → erase_staged_uploads → delete_identity.
var sagaSteps = []user.DeletionStep{
	user.DeletionStepDisableIdentity, user.DeletionStepLogoutSessions,
	user.DeletionStepEraseSkyMail, user.DeletionStepEraseCMS, user.DeletionStepEraseForms,
	user.DeletionStepAnonymizeCore, user.DeletionStepEraseProfile, user.DeletionStepEraseUploads,
	user.DeletionStepDeleteIdentity,
}

// sagaEvents is the order the saga's side effects happen in, across
// Keycloak, the services, core's store and media.
type sagaEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *sagaEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

func (e *sagaEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

func (e *sagaEvents) count(event string) int {
	n := 0
	for _, got := range e.list() {
		if got == event {
			n++
		}
	}
	return n
}

// sagaIdentity is Keycloak for the whole saga: the lifecycle calls and the
// read-only address lookup, with the user disabled but present until the
// last step deletes it.
type sagaIdentity struct {
	events     *sagaEvents
	mu         sync.Mutex
	exists     bool
	enabled    bool
	addresses  []string
	addressErr error
}

func (i *sagaIdentity) EnsureDisabled(context.Context, uuid.UUID) error {
	i.events.add("disable_identity")
	i.mu.Lock()
	defer i.mu.Unlock()
	i.enabled = false
	return nil
}

func (i *sagaIdentity) EnsureLoggedOut(context.Context, uuid.UUID) error {
	i.events.add("logout_sessions")
	return nil
}

func (i *sagaIdentity) EnsureDeleted(context.Context, uuid.UUID) error {
	i.events.add("delete_identity")
	i.mu.Lock()
	defer i.mu.Unlock()
	i.exists = false
	return nil
}

func (i *sagaIdentity) UserAddresses(context.Context, uuid.UUID) ([]string, error) {
	i.events.add("read_addresses")
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.addressErr != nil {
		return nil, i.addressErr
	}
	if !i.exists {
		return nil, nil
	}
	return append([]string(nil), i.addresses...), nil
}

func (i *sagaIdentity) state() (exists, enabled bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.exists, i.enabled
}

func (i *sagaIdentity) set(addresses []string, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.addresses, i.addressErr = addresses, err
}

// sagaStore records when core is anonymized.
type sagaStore struct {
	erasureTestStore
	events *sagaEvents
}

func (s sagaStore) AnonymizeAccount(ctx context.Context, id uuid.UUID, at time.Time, emails []string) error {
	s.events.add("anonymize_core")
	return s.erasureTestStore.AnonymizeAccount(ctx, id, at, emails)
}

type sagaMedia struct{ events *sagaEvents }

func (m sagaMedia) EnsureErased(context.Context, uuid.UUID, time.Time) error {
	m.events.add("erase_profile_media")
	return nil
}

func (m sagaMedia) EnsureSubjectUploadsErased(context.Context, uuid.UUID, time.Time) error {
	m.events.add("erase_staged_uploads")
	return nil
}

const (
	sagaSchool   = "ada.lovelace@std.yildiz.edu.tr"
	sagaPersonal = "ada@example.com"
)

type sagaFixture struct {
	*erasureFixture
	events   *sagaEvents
	identity *sagaIdentity
}

// newSagaFixture is a person whose Primary e-mail is the Personal e-mail:
// Keycloak holds both addresses, core's row holds both too.
func newSagaFixture(t *testing.T, store erasureTestStore) *sagaFixture {
	t.Helper()
	return newSagaFixtureFor(t, store, user.Profile{Email: sagaPersonal, SchoolEmail: sagaSchool, FirstName: "Ada", LastName: "Lovelace"},
		[]string{sagaPersonal, sagaSchool, sagaPersonal})
}

func newSagaFixtureFor(t *testing.T, store erasureTestStore, core user.Profile, keycloak []string) *sagaFixture {
	t.Helper()
	f := &sagaFixture{erasureFixture: newErasureFixtureFor(t, store, core), events: &sagaEvents{}}
	f.identity = &sagaIdentity{events: f.events, exists: true, enabled: true, addresses: keycloak}
	for _, service := range erasure.Registry() {
		f.answer(service.Step, answerCompleted(map[string]int64{service.Name + "_rows_erased": 1}))
	}
	return f
}

// answer sets how a service answers and records each call in the saga's
// order.
func (f *sagaFixture) answer(step user.DeletionStep, respond func(http.ResponseWriter, *http.Request)) {
	f.services[step].set(func(w http.ResponseWriter, r *http.Request) {
		f.events.add(string(step))
		respond(w, r)
	})
}

// services is the group as production builds it: every registry entry with
// its own client, and the addresses read from Keycloak and core's row.
func (f *sagaFixture) group() account.ServiceErasure {
	services := f.serviceErasure(nil)
	services.Addresses = account.NewErasureAddresses(f.identity, f.store)
	return services
}

func (f *sagaFixture) sagaWith(services account.ServiceErasure) *account.Worker {
	config := f.config
	config.Services = services
	return account.NewWorker(sagaStore{erasureTestStore: f.store, events: f.events}, f.identity, config, sagaMedia{events: f.events})
}

func (f *sagaFixture) saga() *account.Worker {
	return f.sagaWith(f.group())
}

func (f *sagaFixture) core() user.User {
	f.t.Helper()
	row, err := f.store.Get(context.Background(), f.subjectID)
	if err != nil {
		f.t.Fatal(err)
	}
	return row
}

func (f *sagaFixture) checkpoints() []user.DeletionStep {
	var steps []user.DeletionStep
	for _, step := range sagaSteps {
		if _, ok := f.records()[step]; ok {
			steps = append(steps, step)
		}
	}
	return steps
}

func (f *sagaFixture) sentEmails(step user.DeletionStep, call int) []string {
	f.t.Helper()
	s := f.services[step]
	s.mu.Lock()
	defer s.mu.Unlock()
	var body struct {
		RequestID uuid.UUID `json:"request_id"`
		SubjectID uuid.UUID `json:"subject_id"`
		Emails    []string  `json:"emails"`
	}
	if err := json.Unmarshal([]byte(s.bodies[call]), &body); err != nil {
		f.t.Fatal(err)
	}
	if body.RequestID != f.request.ID || body.SubjectID != f.subjectID {
		f.t.Fatalf("%s call %d sent request %s subject %s", step, call, body.RequestID, body.SubjectID)
	}
	return body.Emails
}

func TestErasureSagaErasesTheServicesAfterLogoutAndBeforeCoreAndTheIdentity(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	if worked, err := f.run(f.saga()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	want := []string{
		"disable_identity", "logout_sessions", "read_addresses",
		"erase_skymail", "erase_cms", "erase_forms",
		"anonymize_core", "erase_staged_uploads", "delete_identity",
	}
	if got := f.events.list(); !slices.Equal(got, want) {
		t.Fatalf("saga order = %v, want %v", got, want)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted || state.CompletedAt == nil {
		t.Fatalf("request = %+v", state)
	}
	if got := f.checkpoints(); !slices.Equal(got, sagaSteps) || len(f.records()) != len(sagaSteps) {
		t.Fatalf("checkpoints = %v, want all nine", got)
	}
	for _, service := range erasure.Registry() {
		if got := f.sentEmails(service.Step, 0); !slices.Equal(got, []string{sagaPersonal, sagaSchool}) {
			t.Fatalf("%s got addresses %v", service.Step, got)
		}
		// Services answer 404 to anything that looks like it came through
		// the public ingress.
		header := f.services[service.Step].headers[0]
		for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip"} {
			if header.Get(name) != "" {
				t.Fatalf("%s command carries %s", service.Step, name)
			}
		}
	}
	if row := f.core(); row.AccountState != user.AccountAnonymized || row.Email != "" || row.SchoolEmail != "" {
		t.Fatalf("core row = %+v", row)
	}
	if exists, _ := f.identity.state(); exists {
		t.Fatal("the Keycloak user was not deleted")
	}
	f.assertNoPersonalData(f.state().LastErrorCode)
}

func TestErasureSagaCheckpointsCarryTheTimeTheirStepFinished(t *testing.T) {
	t.Parallel()
	testCheckpointsCarryTheTimeTheirStepFinished(t, user.NewMemoryStore())
}

// testCheckpointsCarryTheTimeTheirStepFinished: each checkpoint of one pass
// holds the time its own step finished, so the completion proof's step times
// differ and rise in saga order, and the request completes after its last
// step (ticket 15).
func testCheckpointsCarryTheTimeTheirStepFinished(t *testing.T, store erasureTestStore) {
	t.Helper()
	f := newSagaFixture(t, store)
	// Every read of the worker's clock is a second later, as if each step
	// took that long.
	f.config.Now = func() time.Time {
		f.now = f.now.Add(time.Second)
		return f.now
	}
	if worked, err := f.run(f.saga()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	records := f.records()
	var previous time.Time
	for _, step := range sagaSteps {
		record, ok := records[step]
		if !ok {
			t.Fatalf("%s has no checkpoint", step)
		}
		if !record.CompletedAt.After(previous) {
			t.Fatalf("%s checkpointed at %s, not after the step before it (%s)", step, record.CompletedAt, previous)
		}
		previous = record.CompletedAt
	}
	state := f.state()
	if state.Status != user.DeletionRequestCompleted || state.CompletedAt == nil || !state.CompletedAt.After(previous) ||
		!state.UpdatedAt.Equal(*state.CompletedAt) {
		t.Fatalf("request = %+v, want completed after its last checkpoint %s", state, previous)
	}
}

func TestErasureSagaWaitsForEveryServiceBeforeAnonymizingOrDeleting(t *testing.T) {
	t.Parallel()
	testErasureSagaWaitsForEveryService(t, user.NewMemoryStore())
}

// testErasureSagaWaitsForEveryService: while one service is unavailable the
// person is disabled and logged out, core keeps their row and Keycloak keeps
// the disabled user; once the service answers, the request completes without
// repeating a checkpointed step.
func testErasureSagaWaitsForEveryService(t *testing.T, store erasureTestStore) {
	t.Helper()
	f := newSagaFixture(t, store)
	f.answer(user.DeletionStepEraseCMS, answerStatus(http.StatusServiceUnavailable, "60"))
	worker := f.saga()
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	state := f.state()
	if state.Status != user.DeletionRequestPending || state.AttemptCount != 0 || state.LastErrorCode != "erase_cms_failed" ||
		!state.NextAttemptAt.Equal(f.now.Add(time.Minute)) {
		t.Fatalf("waiting request = %+v", state)
	}
	if f.events.count("anonymize_core") != 0 || f.events.count("delete_identity") != 0 || f.events.count("erase_staged_uploads") != 0 {
		t.Fatalf("a step after the services ran: %v", f.events.list())
	}
	if exists, enabled := f.identity.state(); !exists || enabled {
		t.Fatalf("Keycloak user exists=%v enabled=%v, want disabled but present", exists, enabled)
	}
	if row := f.core(); row.AccountState != user.AccountDeletionPending || row.Email != sagaPersonal || row.SchoolEmail != sagaSchool {
		t.Fatalf("core row changed before the services finished: %+v", row)
	}
	want := []user.DeletionStep{user.DeletionStepDisableIdentity, user.DeletionStepLogoutSessions, user.DeletionStepEraseSkyMail, user.DeletionStepEraseForms}
	if got := f.checkpoints(); !slices.Equal(got, want) {
		t.Fatalf("checkpoints = %v, want %v", got, want)
	}

	f.now = state.NextAttemptAt
	f.answer(user.DeletionStepEraseCMS, answerCompleted(map[string]int64{"collection_items_updated": 2}))
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted {
		t.Fatalf("request = %+v", state)
	}
	if got := f.checkpoints(); !slices.Equal(got, sagaSteps) {
		t.Fatalf("checkpoints = %v", got)
	}
	for event, want := range map[string]int{
		"disable_identity": 1, "logout_sessions": 1, "erase_skymail": 1, "erase_forms": 1, "erase_cms": 2,
		"read_addresses": 2, "anonymize_core": 1, "delete_identity": 1,
	} {
		if got := f.events.count(event); got != want {
			t.Fatalf("%s ran %d times, want %d: %v", event, got, want, f.events.list())
		}
	}
	if row := f.core(); row.AccountState != user.AccountAnonymized || row.Email != "" {
		t.Fatalf("core row = %+v", row)
	}
	f.assertNoPersonalData()
}

func TestErasureSagaRefusesToPassAServiceItHasNoSenderFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		services func(*sagaFixture) account.ServiceErasure
		code     string
	}{
		{name: "forms left out", code: "erase_forms_not_configured", services: func(f *sagaFixture) account.ServiceErasure {
			services := f.group()
			services.Steps = services.Steps[:2]
			return services
		}},
		{name: "nothing configured", code: "erase_skymail_not_configured", services: func(*sagaFixture) account.ServiceErasure {
			return account.ServiceErasure{}
		}},
	} {
		f := newSagaFixture(t, user.NewMemoryStore())
		if worked, err := f.run(f.sagaWith(tc.services(f))); !worked || err == nil {
			t.Fatalf("%s: worked=%v err=%v", tc.name, worked, err)
		}
		if state := f.state(); state.Status != user.DeletionRequestManualIntervention || state.LastErrorCode != tc.code {
			t.Fatalf("%s: request = %+v, want manual intervention with %s", tc.name, state, tc.code)
		}
		if got := f.events.list(); !slices.Equal(got, []string{"disable_identity", "logout_sessions"}) {
			t.Fatalf("%s: events = %v", tc.name, got)
		}
		if got := f.checkpoints(); !slices.Equal(got, []user.DeletionStep{user.DeletionStepDisableIdentity, user.DeletionStepLogoutSessions}) {
			t.Fatalf("%s: checkpoints = %v", tc.name, got)
		}
		f.assertNoPersonalData(f.state().LastErrorCode)
	}
}

func TestErasureSagaGoesOnWithCoresRowWhenKeycloakNoLongerKnowsThePerson(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	f.identity.exists = false
	if worked, err := f.run(f.saga()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	for _, service := range erasure.Registry() {
		if got := f.sentEmails(service.Step, 0); !slices.Equal(got, []string{sagaPersonal, sagaSchool}) {
			t.Fatalf("%s got %v, want core's row", service.Step, got)
		}
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted {
		t.Fatalf("request = %+v", state)
	}
}

func TestErasureSagaCallsNoServiceAndKeepsCoreWhileKeycloakIsUnreachable(t *testing.T) {
	t.Parallel()

	f := newSagaFixture(t, user.NewMemoryStore())
	f.identity.set([]string{sagaPersonal, sagaSchool}, errors.New("identity: user address lookup failed with status 503"))
	worker := f.saga()
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestPending || state.LastErrorCode != "erasure_addresses_failed" || state.AttemptCount != 1 {
		t.Fatalf("request = %+v", state)
	}
	if got := f.events.list(); !slices.Equal(got, []string{"disable_identity", "logout_sessions", "read_addresses"}) {
		t.Fatalf("events = %v", got)
	}
	if row := f.core(); row.Email != sagaPersonal {
		t.Fatalf("core row = %+v", row)
	}

	f.identity.set([]string{sagaPersonal, sagaSchool}, nil)
	f.now = f.state().NextAttemptAt
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted {
		t.Fatalf("request = %+v", state)
	}
	f.assertNoPersonalData()
}

// The command's addresses are the union of Keycloak's and core's, without
// repeats, for every shape a person can have.
func TestErasureSagaSendsTheUnionOfKeycloakAndCoreAddresses(t *testing.T) {
	t.Parallel()

	const legacy = "ada.legacy@example.org"
	for _, tc := range []struct {
		name     string
		core     user.Profile
		keycloak []string
		want     []string
	}{
		{
			name: "two addresses, the School e-mail is primary",
			core: user.Profile{Email: sagaSchool, SchoolEmail: sagaSchool}, keycloak: []string{sagaSchool, sagaSchool, sagaPersonal},
			want: []string{sagaSchool, sagaPersonal},
		},
		{
			name: "two addresses, the Personal e-mail is primary",
			core: user.Profile{Email: sagaPersonal, SchoolEmail: sagaSchool}, keycloak: []string{sagaPersonal, sagaSchool, sagaPersonal},
			want: []string{sagaPersonal, sagaSchool},
		},
		{
			name: "three addresses, a primary kept from before Account center v2",
			core: user.Profile{Email: legacy, SchoolEmail: sagaSchool}, keycloak: []string{legacy, sagaSchool, sagaPersonal},
			want: []string{legacy, sagaSchool, sagaPersonal},
		},
		{
			name: "three addresses, core's row still holds the previous primary",
			core: user.Profile{Email: legacy, SchoolEmail: sagaSchool}, keycloak: []string{"Ada@Example.com ", sagaSchool, sagaPersonal},
			want: []string{sagaPersonal, sagaSchool, legacy},
		},
	} {
		tc.core.FirstName = "Ada"
		f := newSagaFixtureFor(t, user.NewMemoryStore(), tc.core, tc.keycloak)
		if worked, err := f.run(f.saga()); !worked || err != nil {
			t.Fatalf("%s: worked=%v err=%v", tc.name, worked, err)
		}
		for _, service := range erasure.Registry() {
			if got := f.sentEmails(service.Step, 0); !slices.Equal(got, tc.want) {
				t.Fatalf("%s: %s got %v, want %v", tc.name, service.Step, got, tc.want)
			}
		}
	}
}

// Not parallel: it captures the process log. Every path a request can take
// logs through the same lines main.go writes, and none carries an address or
// the subject.
func TestErasureSagaLogsNoAddressOrSubject(t *testing.T) {
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(previous) })

	f := newSagaFixture(t, user.NewMemoryStore())
	worker := f.saga()
	gauges := account.NewErasureGauges()
	watchdog := account.NewWatchdog(f.store, gauges, account.WatchdogConfig{AlertAfter: time.Hour, Now: func() time.Time { return f.now }})
	pass := func() {
		t.Helper()
		// The worker's onError line in main.go.
		if _, err := worker.RunOnce(context.Background()); err != nil {
			log.Printf("account erasure worker: %v", err)
			f.errors = append(f.errors, err.Error())
		}
		if err := watchdog.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.now = f.state().NextAttemptAt
	}

	// Keycloak unreachable, with a failure text that quotes the person.
	f.identity.set([]string{sagaPersonal, sagaSchool}, errors.New("identity: user address lookup failed with status 500"))
	pass()
	f.identity.set([]string{sagaPersonal, sagaSchool}, nil)
	// A service that writes the addresses back in its problem body.
	f.answer(user.DeletionStepEraseSkyMail, answerStatus(http.StatusServiceUnavailable, "30"))
	pass()
	f.answer(user.DeletionStepEraseSkyMail, answerStatus(http.StatusUnauthorized, ""))
	pass()
	// A rejection: manual intervention and the watchdog's attention line.
	f.answer(user.DeletionStepEraseSkyMail, answerStatus(http.StatusForbidden, ""))
	pass()
	if state := f.state(); state.Status != user.DeletionRequestManualIntervention || state.LastErrorCode != "erase_skymail_rejected_403" {
		t.Fatalf("request = %+v", state)
	}
	if !strings.Contains(captured.String(), "account_erasure_attention") {
		t.Fatalf("the watchdog wrote no attention line:\n%s", captured.String())
	}
	for _, code := range []string{"erasure_addresses_failed", "erase_skymail_failed", "erase_skymail_rejected_403"} {
		if want := "account erasure worker: account erasure " + code + " request_id=" + f.request.ID.String() + ":"; !strings.Contains(captured.String(), want) {
			t.Fatalf("no worker line %q:\n%s", want, captured.String())
		}
	}

	f.assertNoPersonalData(captured.String(), f.state().LastErrorCode, gauges.Prometheus())
	for _, value := range []string{sagaPersonal, sagaSchool} {
		if strings.Contains(strings.ToLower(captured.String()), value) {
			t.Fatalf("log carries %q:\n%s", value, captured.String())
		}
	}
}
