package membershipterm

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Store keeps the terms, their mails and the job's runs
// (PostgresStore).
type Store interface {
	// Open are the terms whose membership has not ended, by end then
	// path.
	Open(ctx context.Context) ([]Term, error)
	// OpenFor are the person's open terms.
	OpenFor(ctx context.Context, userID uuid.UUID) ([]Term, error)
	// Get is one term, ended or not; ErrNotFound when there is none.
	Get(ctx context.Context, id uuid.UUID) (Term, error)
	// Insert adds terms; one whose person and Group already have an open
	// term is left out. It answers how many it added.
	Insert(ctx context.Context, terms []Term) (int, error)
	// Restart ends the person's open term of the Group, if any, and opens
	// t in its place: an add through core starts a new term.
	Restart(ctx context.Context, t Term) error
	// End ends the open term of the person and Group at at.
	End(ctx context.Context, userID, groupID uuid.UUID, at time.Time) (int, error)
	// EndIDs ends the open terms ids at at.
	EndIDs(ctx context.Context, ids []uuid.UUID, at time.Time) (int, error)
	// Repath writes the Group's current path on its open terms (a Group
	// renamed since); it answers how many changed.
	Repath(ctx context.Context, groupID uuid.UUID, path string, at time.Time) (int, error)
	// Date gives every open term without an end the end endsAt and the
	// season.
	Date(ctx context.Context, endsAt time.Time, seasonID uuid.UUID, at time.Time) (int, error)
	// Extend gives the open term id a new end (and season), and clears
	// its warnings and its expiry mark. ErrNotFound when it has ended.
	Extend(ctx context.Context, id uuid.UUID, endsAt time.Time, seasonID *uuid.UUID, at time.Time) error
	// Warn marks the open, unexpired term id warned (notice is
	// NoticeWarning14d or NoticeWarning1d) and queues mails, in one
	// transaction; false, and nothing queued, when it already was. A 1-day
	// warning marks the 14-day one too: it is not sent after.
	Warn(ctx context.Context, id uuid.UUID, notice Notice, at time.Time, mails []Mail) (bool, error)
	// Expire marks the open, unmarked terms ids expired at at and queues
	// one expired list of them to each of recipients, in one transaction.
	// It answers the ids it marked.
	Expire(ctx context.Context, ids []uuid.UUID, at time.Time, recipients []uuid.UUID) ([]uuid.UUID, error)
	// Enqueue queues mails.
	Enqueue(ctx context.Context, mails []Mail) error

	// Claim, Complete, Retry, DropBefore and Backlog work the mail queue
	// as the membership mail's queue does.
	Claim(ctx context.Context, now time.Time, lease time.Duration) (*Mail, error)
	Complete(ctx context.Context, m Mail) error
	Retry(ctx context.Context, m Mail, at time.Time) error
	DropBefore(ctx context.Context, before time.Time) (int, error)
	Backlog(ctx context.Context, now time.Time) (int, time.Duration, error)

	// Lock takes the job's lock without waiting: false when another run
	// holds it. release gives it back.
	Lock(ctx context.Context) (release func(), ok bool, err error)
	// LastSuccess is when the latest run of mode and trigger that ended ok
	// started.
	LastSuccess(ctx context.Context, mode Mode, trigger Trigger) (*time.Time, error)
	// LastWeekly is when the latest apply run that looked at the weekly
	// summary (sent or found nothing to send) started.
	LastWeekly(ctx context.Context) (*time.Time, error)
	// StartRun records a running run, marking any run still running
	// abandoned (the lock is held).
	StartRun(ctx context.Context, r Run) error
	// FinishRun records how the run ended, with its counts.
	FinishRun(ctx context.Context, r Run) error
	// Counts are the gauges of /v1/metrics, read at now.
	Counts(ctx context.Context, now time.Time) (Counts, error)
}

// Trigger is who started a run.
type Trigger string

const (
	TriggerSchedule Trigger = "schedule"
	TriggerCLI      Trigger = "cli"
)

// Weekly is what a run did about the weekly summary.
type Weekly string

const (
	WeeklySent   Weekly = "sent"
	WeeklyNone   Weekly = "none"
	WeeklyNotDue Weekly = "not_due"
	WeeklyOff    Weekly = "off"
)

// Run is one run of the daily job, as recorded: counts only.
type Run struct {
	ID          uuid.UUID
	Mode        Mode
	Trigger     Trigger
	StartedAt   time.Time
	FinishedAt  time.Time
	Status      string
	ErrorCode   string
	SeasonFound bool
	// Backfilled are memberships found without a term and given one;
	// Dated, terms without an end given the season's; Ended, terms whose
	// membership was found gone. In a dry run, what apply would do.
	Backfilled int
	Dated      int
	Ended      int
	Warned14d  int
	Warned1d   int
	Expired    int
	// Mails are the mails queued (would be, in a dry run).
	Mails  int
	Weekly Weekly
}

// Counts are the gauges, read from the terms: nothing kept by a process,
// so every replica reports the same.
type Counts struct {
	Open     int
	Undated  int
	Expiring int
	// ExpiredInGroup are open terms past their end: the person is still in
	// the Group (as last seen by the daily job).
	ExpiredInGroup int
	LastSuccess    *time.Time
	LastRunFailed  bool
	SeasonFound    bool
}
