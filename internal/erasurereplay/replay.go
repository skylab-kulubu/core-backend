// Package erasurereplay erases again, in a service restored from a dump taken
// at time T, the people whose deletion requests completed at or after T
// (ADR-0053).
//
// Core keeps no address of an erased person. The restore brings back the
// person's e-mail-keyed data in the service, so the replay reads the
// addresses from the core and Keycloak snapshots taken with the service dump
// at T, the same way the live saga reads them, and sends the service its
// normal Erasure command. The addresses live in memory for one request's call
// and nowhere else; no output, record or error of this package carries an
// address, a name or a subject id, only request ids.
package erasurereplay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// DefaultStepTimeout bounds one service call, as the worker's step timeout
// does in production.
const DefaultStepTimeout = 20 * time.Second

// Request is one deletion request, by its ids alone.
type Request struct {
	ID        uuid.UUID
	SubjectID uuid.UUID
}

// Requests is what the live core database tells the replay: nothing but
// request and subject ids.
type Requests interface {
	// CompletedSince lists the requests completed at or after t.
	CompletedSince(ctx context.Context, t time.Time) ([]Request, error)
	// OpenWithStepSince lists the requests not completed yet whose step was
	// checkpointed at or after t.
	OpenWithStepSince(ctx context.Context, step user.DeletionStep, t time.Time) ([]uuid.UUID, error)
}

// CoreSnapshot reads the person's `users` row from the core snapshot. A row
// the snapshot does not hold, or holds anonymized, is user.ErrNotFound.
type CoreSnapshot interface {
	account.CoreUsers
	// Check refuses a snapshot that is not a core database with people in it.
	Check(context.Context) error
}

// KeycloakSnapshot reads the person's `email`, `schoolEmail` and
// `personalEmail` from the Keycloak snapshot. A user the snapshot does not
// hold is identity.ErrNotFound.
type KeycloakSnapshot interface {
	account.IdentityAddresses
	// Check refuses a snapshot that holds no user of the realm.
	Check(context.Context) error
}

// Outcome is what the replay did for one request.
type Outcome string

const (
	// OutcomeResolved: dry run; the addresses were read and nothing was sent.
	OutcomeResolved Outcome = "resolved"
	// OutcomeDone: the service answered 200.
	OutcomeDone Outcome = "done"
	// OutcomeRetryLater: the service answered 202, or the request is not
	// completed yet; run the replay again later.
	OutcomeRetryLater Outcome = "retry_later"
	// OutcomeFail: the request was not replayed.
	OutcomeFail Outcome = "fail"
)

// Codes of the records that are not done.
const (
	CodeSubjectMissing       = "subject_missing"
	CodeAddressesUnreadable  = "addresses_unreadable"
	CodeInProgress           = "in_progress"
	CodeRequestNotCompleted  = "request_not_completed"
	codeServiceFailureSuffix = "_failed"
)

// Record is the replay of one request, without personal data: the request
// id, the service, the outcome and, when it is not done, a fixed code and
// the reason the erasure client or the address read gave, which name neither
// the subject nor an address. A done record carries the service's counts.
type Record struct {
	At        time.Time
	Service   string
	DumpedAt  time.Time
	RequestID uuid.UUID
	Outcome   Outcome
	Code      string
	Reason    string
	Counts    map[string]int64
}

// String is the record's log line.
func (r Record) String() string {
	var line strings.Builder
	fmt.Fprintf(&line, "account_erasure_replay at=%s service=%s dumped_at=%s request_id=%s outcome=%s",
		r.At.UTC().Format(time.RFC3339), r.Service, r.DumpedAt.UTC().Format(time.RFC3339Nano), r.RequestID, r.Outcome)
	if r.Code != "" {
		fmt.Fprintf(&line, " code=%s", r.Code)
	}
	if r.Reason != "" {
		fmt.Fprintf(&line, " reason=%q", r.Reason)
	}
	if r.Outcome == OutcomeDone {
		keys := make([]string, 0, len(r.Counts))
		for key := range r.Counts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		counts := make([]string, 0, len(keys))
		for _, key := range keys {
			counts = append(counts, fmt.Sprintf("%s:%d", key, r.Counts[key]))
		}
		fmt.Fprintf(&line, " counts=%s", strings.Join(counts, ","))
	}
	return line.String()
}

// Report counts one run.
type Report struct {
	// Requests is the number of requests completed at or after the dump.
	Requests int
	// ByAddresses[n] is the number of requests with n addresses resolved.
	ByAddresses [erasure.MaxAddresses + 1]int
	// Missing are the requests whose subject neither snapshot holds.
	Missing int
	// Unreadable are the requests whose addresses could not be read.
	Unreadable int
	// Open are the requests not completed yet whose step for the service
	// ran at or after the dump.
	Open int
	// Done, RetryLater and SendFailed count what the service answered.
	Done, RetryLater, SendFailed int
}

// Failed is the number of requests that were not and will not be replayed
// without an operator.
func (r Report) Failed() int { return r.Missing + r.Unreadable + r.SendFailed }

// Replay replays erasure into one restored service.
type Replay struct {
	Service erasure.Service
	// DumpedAt is T, when the restored service's dump was taken (its
	// start), not when it was restored.
	DumpedAt time.Time
	Requests Requests
	Core     CoreSnapshot
	Keycloak KeycloakSnapshot
	// Apply sends the Erasure command; without it nothing is sent.
	Apply bool
	// Sender is the service's Erasure command client; needed with Apply.
	Sender account.ErasureSender
	// Record receives one record per request, the requests still open
	// included. Nil drops them.
	Record      func(Record)
	StepTimeout time.Duration
	Now         func() time.Time
}

// Run replays every request completed at or after the dump, one at a time.
// An error means nothing was sent: the live requests or a snapshot could not
// be read at all. Every request after that gets a record and a count; none
// is skipped.
func (r Replay) Run(ctx context.Context) (Report, error) {
	if r.Requests == nil || r.Core == nil || r.Keycloak == nil {
		return Report{}, errors.New("erasure replay: requests or snapshots not configured")
	}
	if r.Apply && r.Sender == nil {
		return Report{}, errors.New("erasure replay: no Erasure command client to apply with")
	}
	if err := r.Core.Check(ctx); err != nil {
		return Report{}, err
	}
	if err := r.Keycloak.Check(ctx); err != nil {
		return Report{}, err
	}
	requests, err := r.Requests.CompletedSince(ctx, r.DumpedAt)
	if err != nil {
		return Report{}, err
	}
	open, err := r.Requests.OpenWithStepSince(ctx, r.Service.Step, r.DumpedAt)
	if err != nil {
		return Report{}, err
	}

	report := Report{Requests: len(requests), Open: len(open)}
	for _, request := range requests {
		r.record(r.replay(ctx, request, &report))
	}
	for _, id := range open {
		r.record(Record{RequestID: id, Outcome: OutcomeRetryLater, Code: CodeRequestNotCompleted})
	}
	return report, nil
}

func (r Replay) replay(ctx context.Context, request Request, report *Report) Record {
	record := Record{RequestID: request.ID}
	emails, found, err := r.addresses(ctx, request.SubjectID)
	switch {
	case err != nil:
		report.Unreadable++
		record.Outcome, record.Code, record.Reason = OutcomeFail, CodeAddressesUnreadable, err.Error()
		return record
	case !found:
		report.Missing++
		record.Outcome, record.Code = OutcomeFail, CodeSubjectMissing
		return record
	}
	report.ByAddresses[len(emails)]++
	if !r.Apply {
		record.Outcome = OutcomeResolved
		return record
	}

	stepCtx, cancel := context.WithTimeout(ctx, r.stepTimeout())
	result, err := r.Sender.Erase(stepCtx, erasure.Command{RequestID: request.ID, SubjectID: request.SubjectID, Emails: emails})
	cancel()
	var deferred *erasure.DeferredError
	var rejected interface{ PermanentCode() string }
	switch {
	case err == nil:
		report.Done++
		record.Outcome, record.Counts = OutcomeDone, result.Counts
	case errors.As(err, &deferred) && deferred.Status == http.StatusAccepted:
		report.RetryLater++
		record.Outcome, record.Code, record.Reason = OutcomeRetryLater, CodeInProgress, err.Error()
	case errors.As(err, &rejected):
		report.SendFailed++
		record.Outcome, record.Code, record.Reason = OutcomeFail, rejected.PermanentCode(), err.Error()
	default:
		report.SendFailed++
		record.Outcome, record.Code, record.Reason = OutcomeFail, string(r.Service.Step)+codeServiceFailureSuffix, err.Error()
	}
	return record
}

// addresses reads the person's addresses from the snapshots through the live
// saga's own union (account.NewErasureAddresses): Keycloak's `email`,
// `schoolEmail` and `personalEmail` with core's `users.email` and
// `users.school_email`, trimmed, lower-cased, without blanks or repeats, at
// most three. found is false when neither snapshot holds the person.
func (r Replay) addresses(ctx context.Context, subject uuid.UUID) (emails []string, found bool, err error) {
	seen := &presence{core: r.Core, keycloak: r.Keycloak}
	emails, err = account.NewErasureAddresses(seen, seen).ErasureAddresses(ctx, subject)
	return emails, seen.inCore || seen.inKeycloak, err
}

// presence hands the snapshots to the live saga's address source and notes
// which of them holds the person. Like the live identity adapter, it turns a
// user Keycloak does not hold into no addresses.
type presence struct {
	core               CoreSnapshot
	keycloak           KeycloakSnapshot
	inCore, inKeycloak bool
}

func (p *presence) UserAddresses(ctx context.Context, id uuid.UUID) ([]string, error) {
	addresses, err := p.keycloak.UserAddresses(ctx, id)
	if errors.Is(err, identity.ErrNotFound) {
		return nil, nil
	}
	p.inKeycloak = err == nil
	return addresses, err
}

func (p *presence) Get(ctx context.Context, id uuid.UUID) (user.User, error) {
	row, err := p.core.Get(ctx, id)
	p.inCore = err == nil
	return row, err
}

func (r Replay) record(record Record) {
	if r.Record == nil {
		return
	}
	record.At = r.now()
	record.Service = r.Service.Name
	record.DumpedAt = r.DumpedAt
	r.Record(record)
}

func (r Replay) stepTimeout() time.Duration {
	if r.StepTimeout > 0 {
		return r.StepTimeout
	}
	return DefaultStepTimeout
}

func (r Replay) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
