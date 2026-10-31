package account_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The deletion notices (docs/account-lifecycle.md, "Deletion notices"): the
// person is told once that their request was received, once SkyMail has
// erased them, and once that their account was deleted, with the addresses
// read before the identity goes. A notice that fails never holds the
// erasure back.

const (
	receivedKey  = mail.DefaultAccountDeletionReceivedTemplateKey
	completedKey = mail.DefaultAccountDeletionCompletedTemplateKey
)

type sentNotice struct {
	key, recipient string
	vars           map[string]string
}

// noticeMailer is SkyMail for the notices: each send is a saga event.
type noticeMailer struct {
	events *sagaEvents
	mu     sync.Mutex
	sent   []sentNotice
	err    error
}

func (m *noticeMailer) AccountDeletionNotice(_ context.Context, key, recipient string, vars map[string]string) error {
	m.events.add("notice:" + key)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentNotice{key: key, recipient: recipient, vars: vars})
	return m.err
}

func (m *noticeMailer) of(key string) []sentNotice {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sentNotice
	for _, notice := range m.sent {
		if notice.key == key {
			out = append(out, notice)
		}
	}
	return out
}

func recipients(notices []sentNotice) []string {
	out := make([]string, 0, len(notices))
	for _, notice := range notices {
		out = append(out, notice.recipient)
	}
	return out
}

// noticeLog keeps the lines the notices write.
type noticeLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *noticeLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *noticeLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (f *sagaFixture) notices(mailer *noticeMailer, logs *noticeLog) *account.DeletionNotices {
	f.t.Helper()
	claims, ok := f.store.(account.DeletionNoticeClaims)
	if !ok {
		f.t.Fatalf("%T claims no notice", f.store)
	}
	return &account.DeletionNotices{
		Claims: claims, Mailer: mailer, ReceivedTemplateKey: receivedKey, CompletedTemplateKey: completedKey, Logf: logs.logf,
	}
}

func TestDeletionNoticesGoOnceEachInTheirPlace(t *testing.T) {
	t.Parallel()
	testDeletionNoticesGoOnceEachInTheirPlace(t, user.NewMemoryStore())
}

func testDeletionNoticesGoOnceEachInTheirPlace(t *testing.T, store erasureTestStore) {
	t.Helper()
	f := newSagaFixture(t, store)
	mailer, logs := &noticeMailer{events: f.events}, &noticeLog{}
	f.config.Notices = f.notices(mailer, logs)
	worker := f.saga()
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	want := []string{
		"disable_identity", "logout_sessions", "read_addresses", "erase_contact_consents", "erase_skypass_wallet",
		"erase_communication_preferences", "erase_skymail", "erase_cms", "erase_forms",
		"notice:" + receivedKey, "notice:" + receivedKey,
		"anonymize_core", "erase_staged_uploads", "delete_identity",
		"notice:" + completedKey, "notice:" + completedKey,
	}
	if got := f.events.list(); !slices.Equal(got, want) {
		t.Fatalf("saga = %v, want %v", got, want)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted {
		t.Fatalf("request = %+v", state)
	}
	received, completed := mailer.of(receivedKey), mailer.of(completedKey)
	addresses := []string{sagaPersonal, sagaSchool}
	if got := recipients(received); !slices.Equal(got, addresses) {
		t.Fatalf("received went to %v, want %v", got, addresses)
	}
	if got := recipients(completed); !slices.Equal(got, addresses) {
		t.Fatalf("completed went to %v, want %v", got, addresses)
	}
	deadline := account.DeletionDeadlineDay(f.request.CreatedAt)
	for _, notice := range received {
		if notice.vars["Deadline"] != deadline || len(notice.vars) != 1 {
			t.Fatalf("received vars %v, want Deadline %s", notice.vars, deadline)
		}
	}
	for _, notice := range completed {
		if len(notice.vars) != 0 {
			t.Fatalf("completed vars %v, want none", notice.vars)
		}
	}
	// A request completed is never claimed again, and a pass over it sends
	// nothing.
	if worked, err := f.run(worker); worked || err != nil {
		t.Fatalf("second pass worked=%v err=%v", worked, err)
	}
	if n := len(mailer.sent); n != 4 {
		t.Fatalf("%d notices after a second pass, want 4", n)
	}
	f.assertNoPersonalData(logs.all()...)
}

func TestDeletionDeadlineDayIsThirtyDaysLaterInIstanbul(t *testing.T) {
	t.Parallel()
	// 21:30 UTC is already the next day in Istanbul.
	if got := account.DeletionDeadlineDay(time.Date(2026, time.October, 31, 21, 30, 0, 0, time.UTC)); got != "01.12.2026" {
		t.Fatalf("deadline = %s, want 01.12.2026", got)
	}
	if got := account.DeletionDeadlineDay(time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)); got != "30.11.2026" {
		t.Fatalf("deadline = %s, want 30.11.2026", got)
	}
}

// The received notice waits for SkyMail's erasure: sent after it, it is the
// mail SkyMail keeps no record of. A pass whose SkyMail call fails sends
// nothing; the pass that erases sends it once, and later passes do not.
func TestDeletionNoticeReceivedWaitsForSkyMailAndGoesOnce(t *testing.T) {
	t.Parallel()
	f := newSagaFixture(t, user.NewMemoryStore())
	mailer, logs := &noticeMailer{events: f.events}, &noticeLog{}
	f.config.Notices = f.notices(mailer, logs)
	f.answer(user.DeletionStepEraseSkyMail, answerStatus(http.StatusServiceUnavailable, "60"))
	f.answer(user.DeletionStepEraseCMS, answerStatus(http.StatusServiceUnavailable, "60"))
	worker := f.saga()
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if n := len(mailer.sent); n != 0 {
		t.Fatalf("%d notices before SkyMail erased the person", n)
	}

	f.now = f.now.Add(time.Minute)
	f.answer(user.DeletionStepEraseSkyMail, answerCompleted(map[string]int64{"queue_rows_cleared": 1}))
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if got := recipients(mailer.of(receivedKey)); !slices.Equal(got, []string{sagaPersonal, sagaSchool}) {
		t.Fatalf("received went to %v", got)
	}

	// The CMS is still down: later passes go through the service group
	// again and send nothing more.
	for i := 0; i < 2; i++ {
		f.now = f.now.Add(time.Minute)
		if worked, err := f.run(worker); !worked || err == nil {
			t.Fatalf("worked=%v err=%v", worked, err)
		}
	}
	f.now = f.now.Add(time.Minute)
	f.answer(user.DeletionStepEraseCMS, answerCompleted(map[string]int64{"actor_columns_replaced": 1}))
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if received, completed := len(mailer.of(receivedKey)), len(mailer.of(completedKey)); received != 2 || completed != 2 {
		t.Fatalf("received %d, completed %d; want 2 and 2", received, completed)
	}
	f.assertNoPersonalData(logs.all()...)
}

// A notice SkyMail refuses is logged, without an address, and never tried
// again; the erasure completes as if it had gone.
func TestDeletionNoticeThatFailsDoesNotHoldTheErasure(t *testing.T) {
	t.Parallel()
	f := newSagaFixture(t, user.NewMemoryStore())
	mailer, logs := &noticeMailer{events: f.events, err: &mail.SendError{Status: http.StatusServiceUnavailable}}, &noticeLog{}
	f.config.Notices = f.notices(mailer, logs)
	worker := f.saga()
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted || len(f.records()) != len(sagaSteps) {
		t.Fatalf("request = %+v with %d checkpoints", state, len(f.records()))
	}
	if exists, _ := f.identity.state(); exists {
		t.Fatal("the identity was not deleted")
	}
	if n := len(mailer.sent); n != 4 {
		t.Fatalf("%d sends, want each address tried once per notice", n)
	}
	lines := logs.all()
	if len(lines) != 2 {
		t.Fatalf("log %v, want one line per notice", lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, "sent=0 failed=2: mail: SkyMail answered 503") {
			t.Fatalf("line %q", line)
		}
	}
	f.assertNoPersonalData(lines...)
}

// The completed notice's addresses are read before the identity is deleted,
// even in a pass that only deletes it: after it Keycloak knows no one and
// core's row is anonymized.
func TestDeletionNoticeCompletedReadsTheAddressesBeforeTheIdentityGoes(t *testing.T) {
	t.Parallel()
	f := newSagaFixture(t, user.NewMemoryStore())
	mailer, logs := &noticeMailer{events: f.events}, &noticeLog{}
	f.config.Notices = f.notices(mailer, logs)
	identity := &deleteOnceFailing{sagaIdentity: f.identity}
	config := f.config
	config.Services = f.group()
	config.ContactConsents = f.consents()
	config.SkyPassWallet = sagaWallet{events: f.events}
	config.CommunicationPreferences = sagaCommunicationPreferences{events: f.events}
	worker := account.NewWorkerWithConfiguredWaits(sagaStore{erasureTestStore: f.store, events: f.events}, identity, config, sagaMedia{events: f.events})
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if n := len(mailer.of(completedKey)); n != 0 {
		t.Fatalf("%d completed notices before the identity was deleted", n)
	}

	before := len(f.events.list())
	f.now = f.now.Add(time.Minute)
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	want := []string{"read_addresses", "delete_identity", "notice:" + completedKey, "notice:" + completedKey}
	if got := f.events.list()[before:]; !slices.Equal(got, want) {
		t.Fatalf("last pass = %v, want %v", got, want)
	}
	if got := recipients(mailer.of(completedKey)); !slices.Equal(got, []string{sagaPersonal, sagaSchool}) {
		t.Fatalf("completed went to %v", got)
	}
	if n := len(mailer.of(receivedKey)); n != 2 {
		t.Fatalf("received sent %d times, want once per address", n)
	}
}

// A Keycloak that cannot be read before the identity is deleted leaves the
// completed notice without addresses; the erasure goes on.
func TestDeletionNoticeWithoutAddressesDoesNotHoldTheErasure(t *testing.T) {
	t.Parallel()
	f := newSagaFixture(t, user.NewMemoryStore())
	mailer, logs := &noticeMailer{events: f.events}, &noticeLog{}
	f.config.Notices = f.notices(mailer, logs)
	identity := &deleteOnceFailing{sagaIdentity: f.identity}
	config := f.config
	config.Services = f.group()
	config.ContactConsents = f.consents()
	config.SkyPassWallet = sagaWallet{events: f.events}
	config.CommunicationPreferences = sagaCommunicationPreferences{events: f.events}
	worker := account.NewWorkerWithConfiguredWaits(sagaStore{erasureTestStore: f.store, events: f.events}, identity, config, sagaMedia{events: f.events})
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	f.identity.set(nil, errors.New("keycloak unavailable"))
	f.now = f.now.Add(time.Minute)
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted {
		t.Fatalf("request = %+v", state)
	}
	if n := len(mailer.of(completedKey)); n != 0 {
		t.Fatalf("%d completed notices without addresses", n)
	}
	f.assertNoPersonalData(logs.all()...)
}

// Off (no Notices, or an empty key) sends nothing.
func TestDeletionNoticesOff(t *testing.T) {
	t.Parallel()
	f := newSagaFixture(t, user.NewMemoryStore())
	mailer, logs := &noticeMailer{events: f.events}, &noticeLog{}
	notices := f.notices(mailer, logs)
	notices.ReceivedTemplateKey, notices.CompletedTemplateKey = "", " "
	f.config.Notices = notices
	if worked, err := f.run(f.saga()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if n := len(mailer.sent); n != 0 {
		t.Fatalf("%d notices with both keys empty", n)
	}
}

// Only the person's own request gets the received notice; an
// administrator's for someone else does not, though the deleted person is
// still told their account was deleted.
func TestDeletionNoticeReceivedIsForTheirOwnRequest(t *testing.T) {
	t.Parallel()
	subject, admin := uuid.New(), uuid.New()
	for _, c := range []struct {
		name string
		by   *uuid.UUID
		want bool
	}{{"Account Center", nil, true}, {"themselves", &subject, true}, {"an administrator", &admin, false}} {
		if got := account.SelfRequested(user.DeletionRequest{SubjectID: subject, RequestedBy: c.by}); got != c.want {
			t.Errorf("%s: self requested %v, want %v", c.name, got, c.want)
		}
	}
}

type deleteOnceFailing struct {
	*sagaIdentity
	mu     sync.Mutex
	failed bool
}

func (i *deleteOnceFailing) EnsureDeleted(ctx context.Context, id uuid.UUID) error {
	i.mu.Lock()
	first := !i.failed
	i.failed = true
	i.mu.Unlock()
	if first {
		i.events.add("delete_identity_failed")
		return errors.New("keycloak delete unavailable")
	}
	return i.sagaIdentity.EnsureDeleted(ctx, id)
}
