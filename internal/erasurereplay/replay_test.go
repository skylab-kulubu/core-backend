package erasurereplay_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/erasurereplay"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var (
	dumpedAt   = time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	replayNow  = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	skymail, _ = erasure.ServiceNamed("skymail")
)

type fakeRequests struct {
	completed []erasurereplay.Request
	open      []uuid.UUID
	since     []time.Time
	steps     []user.DeletionStep
}

func (f *fakeRequests) CompletedSince(_ context.Context, t time.Time) ([]erasurereplay.Request, error) {
	f.since = append(f.since, t)
	return f.completed, nil
}

func (f *fakeRequests) OpenWithStepSince(_ context.Context, step user.DeletionStep, t time.Time) ([]uuid.UUID, error) {
	f.since = append(f.since, t)
	f.steps = append(f.steps, step)
	return f.open, nil
}

// fakeCore is the core snapshot: a person's primary and school e-mail.
type fakeCore struct {
	rows     map[uuid.UUID][2]string
	failFor  uuid.UUID
	checkErr error
}

func (f *fakeCore) Check(context.Context) error { return f.checkErr }

func (f *fakeCore) Get(_ context.Context, id uuid.UUID) (user.User, error) {
	if id == f.failFor {
		return user.User{}, errors.New("core snapshot: users row could not be read")
	}
	row, ok := f.rows[id]
	if !ok {
		return user.User{}, user.ErrNotFound
	}
	return user.User{ID: id, Email: row[0], SchoolEmail: row[1]}, nil
}

// fakeKeycloak is the Keycloak snapshot: email, schoolEmail, personalEmail.
type fakeKeycloak struct {
	users    map[uuid.UUID][]string
	failFor  uuid.UUID
	checkErr error
}

func (f *fakeKeycloak) Check(context.Context) error { return f.checkErr }

func (f *fakeKeycloak) UserAddresses(_ context.Context, id uuid.UUID) ([]string, error) {
	if id == f.failFor {
		return nil, errors.New("keycloak snapshot: user could not be read")
	}
	addresses, ok := f.users[id]
	if !ok {
		return nil, identity.ErrNotFound
	}
	return slices.Clone(addresses), nil
}

type fakeSender struct {
	commands []erasure.Command
	answer   func(erasure.Command) (erasure.Result, error)
}

func (f *fakeSender) Erase(_ context.Context, command erasure.Command) (erasure.Result, error) {
	command.Emails = slices.Clone(command.Emails)
	f.commands = append(f.commands, command)
	if f.answer == nil {
		return erasure.Result{Counts: map[string]int64{"recipients_deleted": 1}}, nil
	}
	return f.answer(command)
}

// people is a restore window of four requests: one person both snapshots
// hold with three distinct addresses, one only Keycloak holds, one only
// core holds, and one neither holds.
type people struct {
	both, keycloakOnly, coreOnly, missing erasurereplay.Request
	requests                              *fakeRequests
	core                                  *fakeCore
	keycloak                              *fakeKeycloak
}

func newPeople() people {
	request := func() erasurereplay.Request {
		return erasurereplay.Request{ID: uuid.New(), SubjectID: uuid.New()}
	}
	p := people{both: request(), keycloakOnly: request(), coreOnly: request(), missing: request()}
	p.requests = &fakeRequests{completed: []erasurereplay.Request{p.both, p.keycloakOnly, p.coreOnly, p.missing}}
	p.core = &fakeCore{rows: map[uuid.UUID][2]string{
		p.both.SubjectID:     {" Ada@Example.com ", "ada.lovelace@std.yildiz.edu.tr"},
		p.coreOnly.SubjectID: {"grace@example.com", ""},
	}}
	p.keycloak = &fakeKeycloak{users: map[uuid.UUID][]string{
		p.both.SubjectID:         {"ada@example.com", "ADA.LOVELACE@std.yildiz.edu.tr", "ada.legacy@example.org"},
		p.keycloakOnly.SubjectID: {"alan@example.com"},
	}}
	return p
}

func (p people) replay(apply bool, sender *fakeSender, records *[]erasurereplay.Record) erasurereplay.Replay {
	replay := erasurereplay.Replay{
		Service: skymail, DumpedAt: dumpedAt,
		Requests: p.requests, Core: p.core, Keycloak: p.keycloak,
		Apply:  apply,
		Now:    func() time.Time { return replayNow },
		Record: func(record erasurereplay.Record) { *records = append(*records, record) },
	}
	if sender != nil {
		replay.Sender = sender
	}
	return replay
}

func TestDryRunCountsTheResolvableAddressesAndSendsNothing(t *testing.T) {
	t.Parallel()

	p := newPeople()
	sender := &fakeSender{}
	var records []erasurereplay.Record
	report, err := p.replay(false, sender, &records).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.commands) != 0 {
		t.Fatalf("a dry run sent %d commands", len(sender.commands))
	}
	want := erasurereplay.Report{Requests: 4, ByAddresses: [4]int{0, 2, 0, 1}, Missing: 1}
	if report != want {
		t.Fatalf("report = %+v, want %+v", report, want)
	}
	if report.Failed() != 1 {
		t.Fatalf("failed = %d", report.Failed())
	}
	if len(p.requests.since) != 2 || p.requests.since[0] != dumpedAt || p.requests.steps[0] != user.DeletionStepEraseSkyMail {
		t.Fatalf("live core read since %v for %v", p.requests.since, p.requests.steps)
	}
	outcomes := map[uuid.UUID]erasurereplay.Outcome{}
	for _, record := range records {
		outcomes[record.RequestID] = record.Outcome
	}
	if outcomes[p.both.ID] != erasurereplay.OutcomeResolved || outcomes[p.missing.ID] != erasurereplay.OutcomeFail || len(records) != 4 {
		t.Fatalf("records = %+v", records)
	}
}

func TestApplySendsTheLiveSagasUnionOfBothSnapshots(t *testing.T) {
	t.Parallel()

	p := newPeople()
	sender := &fakeSender{}
	var records []erasurereplay.Record
	report, err := p.replay(true, sender, &records).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Keycloak's addresses first, then core's row, trimmed, lower-cased and
	// without repeats: account.NewErasureAddresses.
	want := []erasure.Command{
		{RequestID: p.both.ID, SubjectID: p.both.SubjectID, Emails: []string{"ada@example.com", "ada.lovelace@std.yildiz.edu.tr", "ada.legacy@example.org"}},
		{RequestID: p.keycloakOnly.ID, SubjectID: p.keycloakOnly.SubjectID, Emails: []string{"alan@example.com"}},
		{RequestID: p.coreOnly.ID, SubjectID: p.coreOnly.SubjectID, Emails: []string{"grace@example.com"}},
	}
	if len(sender.commands) != len(want) {
		t.Fatalf("sent %d commands, want %d", len(sender.commands), len(want))
	}
	for i := range want {
		got := sender.commands[i]
		if got.RequestID != want[i].RequestID || got.SubjectID != want[i].SubjectID || !slices.Equal(got.Emails, want[i].Emails) {
			t.Fatalf("command %d = %+v, want %+v", i, got, want[i])
		}
	}
	if report.Done != 3 || report.Missing != 1 || report.Failed() != 1 {
		t.Fatalf("report = %+v", report)
	}
	for _, record := range records {
		if record.RequestID == p.missing.ID {
			if record.Outcome != erasurereplay.OutcomeFail || record.Code != erasurereplay.CodeSubjectMissing {
				t.Fatalf("missing subject record = %+v", record)
			}
			continue
		}
		if record.Outcome != erasurereplay.OutcomeDone || record.Counts["recipients_deleted"] != 1 {
			t.Fatalf("record = %+v", record)
		}
	}
}

func TestApplyTakes200AsDone202AsRetryLaterAndAnythingElseAsFail(t *testing.T) {
	t.Parallel()

	step := user.DeletionStepEraseSkyMail
	answers := []struct {
		err     error
		outcome erasurereplay.Outcome
		code    string
	}{
		{nil, erasurereplay.OutcomeDone, ""},
		{&erasure.DeferredError{Step: step, Reason: "in progress (202)", Status: 202}, erasurereplay.OutcomeRetryLater, erasurereplay.CodeInProgress},
		{&erasure.DeferredError{Step: step, Reason: "service unavailable (503)", Status: 503}, erasurereplay.OutcomeFail, "erase_skymail_failed"},
		{&erasure.DeferredError{Step: step, Reason: "request failed (timeout)"}, erasurereplay.OutcomeFail, "erase_skymail_failed"},
		{&erasure.RejectedError{Step: step, Status: 404}, erasurereplay.OutcomeFail, "erase_skymail_rejected_404"},
		{&erasure.Error{Step: step, Reason: "token rejected (401)"}, erasurereplay.OutcomeFail, "erase_skymail_failed"},
	}
	requests := &fakeRequests{}
	keycloak := &fakeKeycloak{users: map[uuid.UUID][]string{}}
	byRequest := map[uuid.UUID]int{}
	for i := range answers {
		request := erasurereplay.Request{ID: uuid.New(), SubjectID: uuid.New()}
		requests.completed = append(requests.completed, request)
		keycloak.users[request.SubjectID] = []string{"person@example.com"}
		byRequest[request.ID] = i
	}
	sender := &fakeSender{answer: func(command erasure.Command) (erasure.Result, error) {
		if err := answers[byRequest[command.RequestID]].err; err != nil {
			return erasure.Result{}, err
		}
		return erasure.Result{Counts: map[string]int64{"recipients_deleted": 1}}, nil
	}}
	var records []erasurereplay.Record
	report, err := erasurereplay.Replay{
		Service: skymail, DumpedAt: dumpedAt, Requests: requests, Core: &fakeCore{}, Keycloak: keycloak,
		Apply: true, Sender: sender, Record: func(record erasurereplay.Record) { records = append(records, record) },
	}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Done != 1 || report.RetryLater != 1 || report.SendFailed != 4 || report.Failed() != 4 {
		t.Fatalf("report = %+v", report)
	}
	for _, record := range records {
		want := answers[byRequest[record.RequestID]]
		if record.Outcome != want.outcome || record.Code != want.code {
			t.Fatalf("answer %v: record %+v, want %s %s", want.err, record, want.outcome, want.code)
		}
	}
}

func TestAPersonAnonymizedInTheCoreSnapshotAndAbsentFromKeycloakIsMissing(t *testing.T) {
	t.Parallel()

	// The core snapshot reader answers user.ErrNotFound for an anonymized
	// row; nothing of the person is left to send, so the request fails.
	request := erasurereplay.Request{ID: uuid.New(), SubjectID: uuid.New()}
	sender := &fakeSender{}
	var records []erasurereplay.Record
	report, err := erasurereplay.Replay{
		Service: skymail, DumpedAt: dumpedAt,
		Requests: &fakeRequests{completed: []erasurereplay.Request{request}},
		Core:     &fakeCore{}, Keycloak: &fakeKeycloak{},
		Apply: true, Sender: sender, Record: func(record erasurereplay.Record) { records = append(records, record) },
	}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.commands) != 0 || report.Missing != 1 || report.Failed() != 1 {
		t.Fatalf("sent %d, report %+v", len(sender.commands), report)
	}
	if len(records) != 1 || records[0].RequestID != request.ID || records[0].Code != erasurereplay.CodeSubjectMissing {
		t.Fatalf("records = %+v", records)
	}
}

func TestAddressesThatCannotBeReadFailTheRequestWithoutASend(t *testing.T) {
	t.Parallel()

	tooMany := erasurereplay.Request{ID: uuid.New(), SubjectID: uuid.New()}
	coreDown := erasurereplay.Request{ID: uuid.New(), SubjectID: uuid.New()}
	keycloakDown := erasurereplay.Request{ID: uuid.New(), SubjectID: uuid.New()}
	sender := &fakeSender{}
	var records []erasurereplay.Record
	report, err := erasurereplay.Replay{
		Service: skymail, DumpedAt: dumpedAt,
		Requests: &fakeRequests{completed: []erasurereplay.Request{tooMany, coreDown, keycloakDown}},
		Core: &fakeCore{failFor: coreDown.SubjectID, rows: map[uuid.UUID][2]string{
			tooMany.SubjectID: {"d@example.com", ""},
		}},
		Keycloak: &fakeKeycloak{failFor: keycloakDown.SubjectID, users: map[uuid.UUID][]string{
			tooMany.SubjectID:  {"a@example.com", "b@example.com", "c@example.com"},
			coreDown.SubjectID: {"e@example.com"},
		}},
		Apply: true, Sender: sender, Record: func(record erasurereplay.Record) { records = append(records, record) },
	}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.commands) != 0 || report.Unreadable != 3 || report.Failed() != 3 {
		t.Fatalf("sent %d, report %+v", len(sender.commands), report)
	}
	for _, record := range records {
		if record.Outcome != erasurereplay.OutcomeFail || record.Code != erasurereplay.CodeAddressesUnreadable || record.Reason == "" {
			t.Fatalf("record = %+v", record)
		}
		assertNoPersonalData(t, record.String(), "a@example.com", "d@example.com", "e@example.com",
			tooMany.SubjectID.String(), coreDown.SubjectID.String(), keycloakDown.SubjectID.String())
	}
}

func TestRequestsStillOpenWhoseStepRanAfterTheRestoreAskForAnotherRun(t *testing.T) {
	t.Parallel()

	open := uuid.New()
	p := newPeople()
	p.requests.open = []uuid.UUID{open}
	var records []erasurereplay.Record
	report, err := p.replay(false, nil, &records).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Open != 1 {
		t.Fatalf("report = %+v", report)
	}
	last := records[len(records)-1]
	if last.RequestID != open || last.Outcome != erasurereplay.OutcomeRetryLater || last.Code != erasurereplay.CodeRequestNotCompleted {
		t.Fatalf("open request record = %+v", last)
	}
}

func TestASnapshotThatFailsItsCheckStopsTheReplayBeforeAnything(t *testing.T) {
	t.Parallel()

	for name, broken := range map[string]func(p people){
		"core":     func(p people) { p.core.checkErr = errors.New("core snapshot: the users table is empty") },
		"keycloak": func(p people) { p.keycloak.checkErr = errors.New(`keycloak snapshot: realm "e-skylab" holds no user`) },
	} {
		p := newPeople()
		broken(p)
		sender := &fakeSender{}
		var records []erasurereplay.Record
		if _, err := p.replay(true, sender, &records).Run(context.Background()); err == nil {
			t.Fatalf("%s: a broken snapshot was accepted", name)
		}
		if len(sender.commands) != 0 || len(records) != 0 || len(p.requests.since) != 0 {
			t.Fatalf("%s: sent %d, recorded %d, read live core %d times", name, len(sender.commands), len(records), len(p.requests.since))
		}
	}
}

func TestApplyWithoutASenderIsRefused(t *testing.T) {
	t.Parallel()

	var records []erasurereplay.Record
	if _, err := newPeople().replay(true, nil, &records).Run(context.Background()); err == nil {
		t.Fatal("apply ran without a sender")
	}
}

func TestRecordLinesCarryRequestIDsAndNoPersonalData(t *testing.T) {
	t.Parallel()

	p := newPeople()
	var records []erasurereplay.Record
	if _, err := p.replay(true, &fakeSender{}, &records).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, record := range records {
		lines = append(lines, record.String())
	}
	all := strings.Join(lines, "\n")
	// Only whole addresses and domains: a short name could turn up in a uuid.
	assertNoPersonalData(t, all, "ada@", "ada.l", "alan@", "grace@", "example.com", "example.org", "yildiz.edu.tr",
		p.both.SubjectID.String(), p.keycloakOnly.SubjectID.String(), p.coreOnly.SubjectID.String(), p.missing.SubjectID.String())
	want := "account_erasure_replay at=2026-09-27T10:00:00Z service=skymail dumped_at=2026-09-20T03:00:00Z request_id=" +
		p.both.ID.String() + " outcome=done counts=recipients_deleted:1"
	if lines[0] != want {
		t.Fatalf("line = %q, want %q", lines[0], want)
	}
	if want := "request_id=" + p.missing.ID.String() + " outcome=fail code=subject_missing"; !strings.HasSuffix(lines[3], want) {
		t.Fatalf("line = %q, want suffix %q", lines[3], want)
	}
}

func assertNoPersonalData(t *testing.T, text string, values ...string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, value := range values {
		if strings.Contains(lower, strings.ToLower(value)) {
			t.Fatalf("output carries %q:\n%s", value, text)
		}
	}
}
