package formsfeed

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// States of a ticket job.
const (
	JobQueued  = "queued"
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

const (
	// ticketJobTick is how often the worker looks for due jobs.
	ticketJobTick = 15 * time.Second
	// ticketJobLease keeps a claimed job from another replica.
	ticketJobLease = 5 * time.Minute
	// ticketJobWait is how long a job waits for a gathering to end before
	// it looks again.
	ticketJobWait = 30 * time.Second
	// ticketJobPage is how many rows a job reads at a time.
	ticketJobPage = 100
	// ticketJobMaxAttempts is how often a failing job is tried.
	ticketJobMaxAttempts = 5
	// ticketJobRetention is how long a finished job is kept for the panel.
	ticketJobRetention = 30 * 24 * time.Hour
)

// TicketJob is an organizer's "send them all" (respondent_ticket_jobs):
// counts only.
type TicketJob struct {
	ID        uuid.UUID  `json:"jobId"`
	EventID   uuid.UUID  `json:"-"`
	FormID    *uuid.UUID `json:"formId"`
	State     string     `json:"state"`
	Attempts  int        `json:"-"`
	After     uuid.UUID  `json:"-"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	SendResult
}

// TicketJobQueue keeps the ticket jobs (PostgresTicketJobs).
type TicketJobQueue interface {
	// Enqueue queues a job of the Event (of formID when set) as id and
	// answers id; one waits already: its id.
	Enqueue(ctx context.Context, id, eventID uuid.UUID, formID, requestedBy *uuid.UUID, now time.Time) (uuid.UUID, error)
	// Get answers the Event's job, or ErrTicketJobNotFound.
	Get(ctx context.Context, eventID, id uuid.UUID) (TicketJob, error)
	// Claim takes the next due job, running, leased.
	Claim(ctx context.Context, now time.Time) (TicketJob, bool, error)
	// Progress keeps the counts and the last row handled, and renews the
	// lease.
	Progress(ctx context.Context, job TicketJob, now time.Time) error
	// Postpone gives the job back, queued, until next.
	Postpone(ctx context.Context, id uuid.UUID, attempts int, next time.Time) error
	// Finish ends the job (done or failed) with its counts, and deletes
	// jobs finished long ago.
	Finish(ctx context.Context, job TicketJob, state string, now time.Time) error
}

// PostgresTicketJobs is TicketJobQueue on respondent_ticket_jobs.
type PostgresTicketJobs struct{ pool *pgxpool.Pool }

// NewPostgresTicketJobs builds the queue on pool.
func NewPostgresTicketJobs(pool *pgxpool.Pool) *PostgresTicketJobs {
	return &PostgresTicketJobs{pool: pool}
}

const ticketJobColumns = `id, event_id, form_id, state, attempts, COALESCE(after_id, '00000000-0000-0000-0000-000000000000'::uuid),
	recorded, waitlisted, full_count, skipped, created_at, updated_at`

func scanTicketJob(row pgx.Row) (TicketJob, error) {
	var j TicketJob
	err := row.Scan(&j.ID, &j.EventID, &j.FormID, &j.State, &j.Attempts, &j.After,
		&j.Recorded, &j.Waitlisted, &j.Full, &j.Skipped, &j.CreatedAt, &j.UpdatedAt)
	return j, err
}

func (s *PostgresTicketJobs) Enqueue(ctx context.Context, newID, eventID uuid.UUID, formID, requestedBy *uuid.UUID, now time.Time) (uuid.UUID, error) {
	// The waiting job may finish between the insert that found it and the
	// read: then the insert goes again (a few times at most).
	for range 3 {
		var id uuid.UUID
		err := s.pool.QueryRow(ctx, `
			INSERT INTO respondent_ticket_jobs (id, event_id, form_id, requested_by, next_attempt_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $5, $5)
			ON CONFLICT (event_id, COALESCE(form_id, '00000000-0000-0000-0000-000000000000'::uuid)) WHERE state IN ('queued', 'running')
			DO NOTHING
			RETURNING id`, newID, eventID, formID, requestedBy, now).Scan(&id)
		if !errors.Is(err, pgx.ErrNoRows) {
			return id, err
		}
		err = s.pool.QueryRow(ctx, `
			SELECT id FROM respondent_ticket_jobs
			WHERE event_id = $1 AND form_id IS NOT DISTINCT FROM $2 AND state IN ('queued', 'running')`, eventID, formID).Scan(&id)
		if !errors.Is(err, pgx.ErrNoRows) {
			return id, err
		}
	}
	return uuid.Nil, errors.New("formsfeed: the ticket job could not be queued")
}

func (s *PostgresTicketJobs) Get(ctx context.Context, eventID, id uuid.UUID) (TicketJob, error) {
	j, err := scanTicketJob(s.pool.QueryRow(ctx, `SELECT `+ticketJobColumns+` FROM respondent_ticket_jobs WHERE id = $1 AND event_id = $2`, id, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketJob{}, ErrTicketJobNotFound
	}
	return j, err
}

func (s *PostgresTicketJobs) Claim(ctx context.Context, now time.Time) (TicketJob, bool, error) {
	j, err := scanTicketJob(s.pool.QueryRow(ctx, `
		UPDATE respondent_ticket_jobs SET state = 'running', next_attempt_at = $2, updated_at = $1
		WHERE id = (
			SELECT id FROM respondent_ticket_jobs
			WHERE state IN ('queued', 'running') AND next_attempt_at <= $1
			ORDER BY next_attempt_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1)
		RETURNING `+ticketJobColumns, now, now.Add(ticketJobLease)))
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketJob{}, false, nil
	}
	return j, err == nil, err
}

func (s *PostgresTicketJobs) Progress(ctx context.Context, j TicketJob, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE respondent_ticket_jobs SET recorded = $2, waitlisted = $3, full_count = $4, skipped = $5,
			after_id = NULLIF($6, '00000000-0000-0000-0000-000000000000'::uuid), next_attempt_at = $7, updated_at = $8
		WHERE id = $1`, j.ID, j.Recorded, j.Waitlisted, j.Full, j.Skipped, j.After, now.Add(ticketJobLease), now)
	return err
}

func (s *PostgresTicketJobs) Postpone(ctx context.Context, id uuid.UUID, attempts int, next time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE respondent_ticket_jobs SET state = 'queued', attempts = $2, next_attempt_at = $3 WHERE id = $1`, id, attempts, next)
	return err
}

func (s *PostgresTicketJobs) Finish(ctx context.Context, j TicketJob, state string, now time.Time) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE respondent_ticket_jobs SET state = $2, recorded = $3, waitlisted = $4, full_count = $5, skipped = $6, updated_at = $7
		WHERE id = $1`, j.ID, state, j.Recorded, j.Waitlisted, j.Full, j.Skipped, now); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM respondent_ticket_jobs WHERE state IN ('done', 'failed') AND updated_at < $1`, now.Add(-ticketJobRetention))
	return err
}

// TicketJobs runs the ticket jobs: each sends the Tickets of its Event's
// accepted Unticketed respondents (of its form when set), a page at a time,
// by the one Ticket rule (Respondents.send). A job waits while a gathering
// of its forms runs, so that the people it finds are sent too. It runs
// whether or not EVENTS_FORMS_ENABLED is on: it reads no Forms.
type TicketJobs struct {
	Queue       TicketJobQueue
	Respondents *Respondents
	Now         func() time.Time
	Logf        func(string, ...any)
}

func (t *TicketJobs) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Run works the jobs until ctx ends; the channel closes when it stopped.
func (t *TicketJobs) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(ticketJobTick)
		defer ticker.Stop()
		for {
			for ctx.Err() == nil {
				ran, err := t.RunOne(ctx)
				if err != nil && ctx.Err() == nil && t.Logf != nil {
					t.Logf(`{"event":"respondent_ticket_job_claim_failed","level":"warn","error":%q}`, events.SafeErrorText(err))
				}
				if !ran || err != nil {
					break
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// RunOne claims and runs one due job; false when none was due.
func (t *TicketJobs) RunOne(ctx context.Context) (bool, error) {
	job, ok, err := t.Queue.Claim(ctx, t.now())
	if err != nil || !ok {
		return false, err
	}
	if t.Respondents.Pulls != nil {
		collecting, err := t.Respondents.Pulls.Collecting(ctx, job.EventID, job.FormID)
		if err != nil {
			return true, t.retry(ctx, job, err)
		}
		if collecting {
			return true, t.Queue.Postpone(ctx, job.ID, job.Attempts, t.now().Add(ticketJobWait))
		}
	}
	for ctx.Err() == nil {
		rows, err := t.Respondents.Store.Next(ctx, job.EventID, job.FormID, job.After, ticketJobPage)
		if err != nil {
			return true, t.retry(ctx, job, err)
		}
		if len(rows) == 0 {
			t.log(job, JobDone, nil)
			return true, t.finish(ctx, job, JobDone, nil)
		}
		res, err := t.Respondents.send(ctx, job.EventID, rows)
		job.SendResult.add(res.SendResult)
		if res.handled > 0 {
			job.After = rows[res.handled-1].ID
		}
		if errors.Is(err, ErrRespondentsEventCancelled) || errors.Is(err, ErrRespondentsEventEnded) || errors.Is(err, ErrMappingEventNotFound) {
			t.log(job, JobFailed, err)
			return true, t.finish(ctx, job, JobFailed, err)
		}
		if err != nil {
			// The job goes on after the last row it finished (send counts
			// exactly those rows): each counted once, and a repeat writes no
			// second Ticket.
			return true, t.retry(ctx, job, err)
		}
		if err := t.Queue.Progress(ctx, job, t.now()); err != nil {
			return true, err
		}
	}
	// Shutting down: the lease gives the job back later.
	return true, nil
}

func (t *TicketJobs) retry(ctx context.Context, job TicketJob, cause error) error {
	job.Attempts++
	if job.Attempts >= ticketJobMaxAttempts {
		t.log(job, JobFailed, cause)
		return t.finish(ctx, job, JobFailed, cause)
	}
	t.log(job, "retry", cause)
	if err := t.Queue.Progress(ctx, job, t.now()); err != nil {
		return err
	}
	return t.Queue.Postpone(ctx, job.ID, job.Attempts, t.now().Add(time.Minute<<min(job.Attempts, 6)))
}

// finish ends the job, then writes its counts to the Team audit row its
// request opened (pending while it ran).
func (t *TicketJobs) finish(ctx context.Context, job TicketJob, state string, cause error) error {
	if err := t.Queue.Finish(ctx, job, state, t.now()); err != nil {
		return err
	}
	if t.Respondents.Tickets != nil {
		t.Respondents.Tickets.FinishFormRespondentJob(ctx, job.EventID, job.ID, ticket.FormRespondentSummary{
			Rows:     job.Recorded + job.Waitlisted + job.Full + job.Skipped,
			Recorded: job.Recorded, Waitlisted: job.Waitlisted, Full: job.Full, Skipped: job.Skipped,
		}, cause)
	}
	return nil
}

// log writes the job's line: ids and counts, never who.
func (t *TicketJobs) log(job TicketJob, outcome string, err error) {
	if t.Logf == nil {
		return
	}
	formID := ""
	if job.FormID != nil {
		formID = job.FormID.String()
	}
	reason := ""
	if err != nil {
		reason = events.SafeErrorText(err)
	}
	payload, marshalErr := json.Marshal(struct {
		Event      string `json:"event"`
		JobID      string `json:"job_id"`
		EventID    string `json:"event_id"`
		FormID     string `json:"form_id"`
		Outcome    string `json:"outcome"`
		Reason     string `json:"reason,omitempty"`
		Attempts   int    `json:"attempts"`
		Recorded   int    `json:"recorded"`
		Waitlisted int    `json:"waitlisted"`
		Full       int    `json:"full"`
		Skipped    int    `json:"skipped"`
	}{"respondent_ticket_job", job.ID.String(), job.EventID.String(), formID, outcome, reason, job.Attempts,
		job.Recorded, job.Waitlisted, job.Full, job.Skipped})
	if marshalErr == nil {
		t.Logf("%s", payload)
	}
}
