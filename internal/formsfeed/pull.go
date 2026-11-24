package formsfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/events"
)

const (
	// pullTick is how often the worker looks for due jobs.
	pullTick = 30 * time.Second
	// planEvery is how often the daily pass's planning reads the Events.
	planEvery = time.Hour
	// pullGiveUp is how long a job is tried: a week, as Forms' outbox
	// tries an event (ADR-0071 §6). The daily pass catches up later.
	pullGiveUp = 7 * 24 * time.Hour
	// minPullBackoff and maxPullBackoff bound the wait after a failed try,
	// doubling: a Forms without the list route (404) is asked again at most
	// once a day after the first day.
	minPullBackoff = time.Minute
	maxPullBackoff = 24 * time.Hour
)

// Lister reads a page of a form's accepted answers (Client).
type Lister interface {
	List(ctx context.Context, formID uuid.UUID, q ListQuery) (Page, error)
}

// TxBeginner opens the transaction an answer's version is kept in
// (*pgxpool.Pool).
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Puller runs the pulls: an organizer's backfill and the daily catch-up
// (docs/form-pulls.md). Each answer goes through Recorder.Response, the
// events' and the HTTP report's Ticket rule.
type Puller struct {
	Queue    PullQueue
	Forms    Lister
	Recorder *Recorder
	Events   EventLister
	// DB opens each answer's transaction; nil runs without one (tests).
	DB   TxBeginner
	Now  func() time.Time
	Logf func(string, ...any)

	once     sync.Once
	mu       sync.Mutex
	counts   map[[2]string]int64
	lastPlan time.Time
}

func (p *Puller) init() {
	p.once.Do(func() {
		p.counts = map[[2]string]int64{}
		for _, kind := range []string{PullBackfill, PullCatchup} {
			for _, outcome := range []string{"done", "retry", "abandoned"} {
				p.counts[[2]string{kind, outcome}] = 0
			}
		}
	})
}

func (p *Puller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Puller) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// Run plans and runs the pulls until ctx ends; the channel closes when it
// has stopped. A job running when ctx ends stops at its next answer and is
// taken again after its lease.
func (p *Puller) Run(ctx context.Context) <-chan struct{} {
	p.init()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(pullTick)
		defer ticker.Stop()
		for {
			p.Tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// Tick plans when an hour has passed since the last planning, then runs
// the due jobs one after the other.
func (p *Puller) Tick(ctx context.Context) {
	p.init()
	if now := p.now(); p.lastPlan.IsZero() || now.Sub(p.lastPlan) >= planEvery {
		if n, err := p.Plan(ctx); err != nil {
			if ctx.Err() == nil {
				p.logf(`{"event":"form_pull_plan_failed","level":"warn","error":%q}`, err.Error())
			}
		} else {
			p.lastPlan = now
			if n > 0 {
				p.logf(`{"event":"form_pull_planned","jobs":%d}`, n)
			}
		}
	}
	for ctx.Err() == nil {
		ran, err := p.RunOne(ctx)
		if err != nil && ctx.Err() == nil {
			p.logf(`{"event":"form_pull_claim_failed","level":"warn","error":%q}`, err.Error())
		}
		if !ran || err != nil {
			return
		}
	}
}

// Plan reads the forms the daily pass covers: the application and extra
// forms of the current Events that are neither cancelled nor ended (drafts
// too: their forms write Tickets, ADR 0066). A feedback form is not pulled.
func (p *Puller) Plan(ctx context.Context) (int, error) {
	evs, err := p.Events.ListIncludingDrafts(ctx, "")
	if err != nil {
		return 0, err
	}
	now := p.now()
	seen := map[uuid.UUID]bool{}
	var forms []uuid.UUID
	for _, ev := range evs {
		if ev.ArchivedAt != nil || ev.Status == event.StatusCancelled || ev.EndedAt(now) {
			continue
		}
		for _, id := range applicationForms(ev) {
			if !seen[id] {
				seen[id] = true
				forms = append(forms, id)
			}
		}
	}
	return p.Queue.Plan(ctx, forms, now)
}

// applicationForms are the forms an Event lists (event.Event.ListsForm).
func applicationForms(ev event.Event) []uuid.UUID {
	var out []uuid.UUID
	if id, ok := event.FormIDFromURL(ev.FormURL); ok {
		out = append(out, id)
	}
	for _, extra := range ev.ExtraFormURLs {
		if id, ok := event.FormIDFromURL(extra.URL); ok {
			out = append(out, id)
		}
	}
	return out
}

// pullResult is what one run of a job did, for its log line.
type pullResult struct {
	pages, items, skipped, written, failed int
	outcomes                               map[string]int
}

// RunOne claims and runs one due job; false when none was due.
func (p *Puller) RunOne(ctx context.Context) (bool, error) {
	p.init()
	job, ok, err := p.Queue.Claim(ctx, p.now())
	if err != nil || !ok {
		return false, err
	}
	res := pullResult{outcomes: map[string]int{}}
	watermark, err := p.run(ctx, job, &res)
	if ctx.Err() != nil {
		// Shutting down: the lease returns the job later.
		return true, nil
	}
	now := p.now()
	switch {
	case err == nil:
		if err := p.Queue.Finish(ctx, job, watermark, now); err != nil {
			return true, err
		}
		p.count(job.Kind, "done")
		p.logJob(job, res, "done", nil)
	case now.Sub(job.RequestedAt) >= pullGiveUp:
		if err := p.Queue.Drop(ctx, job.ID); err != nil {
			return true, err
		}
		p.count(job.Kind, "abandoned")
		p.logJob(job, res, "abandoned", err)
	default:
		attempts := job.Attempts + 1
		wait := minPullBackoff << min(attempts-1, 12)
		if wait > maxPullBackoff || wait <= 0 {
			wait = maxPullBackoff
		}
		if err := p.Queue.Retry(ctx, job.ID, attempts, now.Add(wait)); err != nil {
			return true, err
		}
		p.count(job.Kind, "retry")
		job.Attempts = attempts
		p.logJob(job, res, "retry", err)
	}
	return true, nil
}

// run reads the job's pages from its cursor on and hands each answer to the
// recorder. An answer the read did not find, or one the Ticket rule refuses,
// is counted and passed; any other failure stops the job, to be tried again
// from the page it was on (repeating answers is harmless). It answers the
// last page's watermark.
func (p *Puller) run(ctx context.Context, job PullJob, res *pullResult) (*time.Time, error) {
	q := ListQuery{After: job.Cursor}
	if job.Kind == PullCatchup {
		q.ChangedSince = job.ChangedSince
	}
	// The form's role is read once for the job: the Events do not change
	// what an answer is between two of its pages that matters here, and
	// the Ticket rule reads them again for each answer.
	role, err := p.Recorder.role(ctx, job.FormID)
	if err != nil {
		return nil, fmt.Errorf("reading the Events: %w", err)
	}
	origin := Origin{Via: ViaPull, role: role}
	for {
		page, err := p.Forms.List(ctx, job.FormID, q)
		if err != nil {
			return nil, err
		}
		res.pages++
		res.skipped += page.Skipped
		for _, item := range page.Items {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			outcome, err := p.answer(ctx, origin, item)
			res.items++
			res.outcomes[outcome]++
			if outcome == "recorded" || outcome == "waitlisted" {
				res.written++
			}
			if err != nil {
				if !events.IsPermanent(err) {
					return nil, err
				}
				res.failed++
			}
		}
		if page.NextCursor == "" {
			return page.Watermark, nil
		}
		if err := p.Queue.SaveCursor(ctx, job.ID, page.NextCursor); err != nil {
			return nil, err
		}
		q.After = page.NextCursor
	}
}

// answer runs one answer in its own transaction.
func (p *Puller) answer(ctx context.Context, origin Origin, item Response) (string, error) {
	var tx pgx.Tx
	if p.DB != nil {
		var err error
		if tx, err = p.DB.Begin(ctx); err != nil {
			return OutcomeFailed, err
		}
		defer tx.Rollback(ctx)
	}
	outcome, err := p.Recorder.Response(ctx, tx, origin, item, true)
	if err != nil || tx == nil {
		return outcome, err
	}
	return outcome, tx.Commit(ctx)
}

// logJob writes the job's line: counts only, never who answered.
func (p *Puller) logJob(job PullJob, res pullResult, outcome string, err error) {
	eventID := ""
	if job.EventID != nil {
		eventID = job.EventID.String()
	}
	reason := ""
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		reason = "not_found"
	case errors.Is(err, ErrForbidden):
		reason = "forbidden"
	case errors.Is(err, ErrInvalid):
		reason = "invalid"
	default:
		reason = "failed"
	}
	outcomes, _ := json.Marshal(res.outcomes)
	p.logf(`{"event":"form_pull","level":%q,"kind":%q,"form_id":%q,"event_id":%q,"outcome":%q,"reason":%q,"attempts":%d,"pages":%d,"items":%d,"skipped":%d,"tickets_written":%d,"failed":%d,"outcomes":%s}`,
		levelOf(outcome), job.Kind, job.FormID.String(), eventID, outcome, reason, job.Attempts, res.pages, res.items, res.skipped, res.written, res.failed, outcomes)
}

func levelOf(outcome string) string {
	if outcome == "done" {
		return "info"
	}
	return "warn"
}

func (p *Puller) count(kind, outcome string) {
	p.mu.Lock()
	p.counts[[2]string{kind, outcome}]++
	p.mu.Unlock()
}

// Prometheus is skylab_form_pulls_total{kind,outcome="done|retry|abandoned"}.
func (p *Puller) Prometheus() string {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	b.WriteString("# TYPE skylab_form_pulls_total counter\n")
	for _, kind := range []string{PullBackfill, PullCatchup} {
		for _, outcome := range []string{"done", "retry", "abandoned"} {
			fmt.Fprintf(&b, "skylab_form_pulls_total{kind=%q,outcome=%q} %d\n", kind, outcome, p.counts[[2]string{kind, outcome}])
		}
	}
	return b.String()
}
