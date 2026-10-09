package teammail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // EffectiveAt is Istanbul's date whatever the image carries.

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

const (
	// RetryFirst is the wait before a failed send is tried again; it doubles
	// with each failure in a row, up to retryMax.
	RetryFirst = 30 * time.Second
	retryMax   = 30 * time.Minute
	// GiveUpAfter is how long a change is tried. A mail about a membership
	// change that is days old is no longer news; it is dropped and counted.
	GiveUpAfter = 72 * time.Hour
	// lease is how long a pass holds the change it took: another pass
	// (another core) does not take it meanwhile, and a pass that crashed
	// gives it back when it runs out. A pass takes one change at a time, so
	// the lease only has to outlast one send (sendTimeout); the claim fence
	// (Change.ClaimedAt) keeps a late core from touching a row taken again.
	lease = 2 * time.Minute
	// pollInterval is how often the worker looks at the queue when no
	// change wakes it: for retries, and for changes another core queued.
	pollInterval = 15 * time.Second
	// maxSends bounds one pass.
	maxSends = 200
	// sendTimeout bounds one change: the lookups and the SkyMail call.
	sendTimeout = 45 * time.Second
	// enqueueTimeout bounds the insert a membership write waits for, and
	// writeTimeout the row's own update after a send, which runs even when
	// core is stopping: a mail SkyMail took is done with.
	enqueueTimeout = 5 * time.Second
	writeTimeout   = 5 * time.Second
	timeZone       = "Europe/Istanbul"
	dateLayout     = "02.01.2006"
)

// SkipReason says why a change gets no mail.
type SkipReason string

const (
	// SkipInactive: the person is erased, being erased, disabled or gone.
	SkipInactive SkipReason = "inactive"
	// SkipNoEmail: the account has no primary e-mail.
	SkipNoEmail SkipReason = "no_email"
	// SkipNotTeam: the Group is no team by the rule of the core sending it
	// (queued by a core whose rule was wider).
	SkipNotTeam SkipReason = "not_team"
)

// Recipient is who a mail goes to: the primary e-mail and the name.
type Recipient struct {
	Email    string
	FullName string
}

// People reads, when the mail is sent, who it goes to and who made the
// change.
type People interface {
	// Recipient is the person the mail goes to, or why none goes. An error
	// is a lookup that failed; the change is tried again later.
	Recipient(ctx context.Context, id uuid.UUID) (Recipient, SkipReason, error)
	// DisplayName is the full name of the person who made the change, empty
	// when there is none to show.
	DisplayName(ctx context.Context, id uuid.UUID) string
}

// GroupReader reads the team's Group for its display name.
type GroupReader interface {
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
}

// Mailer sends the mail and says whether SkyMail took it (mail.SkyMail).
type Mailer interface {
	TeamMembership(ctx context.Context, recipient, fullName string, vars map[string]string) error
}

// Config is what a Service takes.
type Config struct {
	Queue  Queue
	People People
	Groups GroupReader
	Mail   Mailer
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger hears counts and failures, never a person; nil is log.Default.
	Logger *log.Logger
}

// Service queues team membership changes (identity.MembershipNotifier) and
// sends their mails (Run).
type Service struct {
	queue    Queue
	people   People
	groups   GroupReader
	mail     Mailer
	now      func() time.Time
	logger   *log.Logger
	location *time.Location
	wake     chan struct{}

	mu             sync.Mutex
	enqueued       int64
	coalesced      map[Enqueued]int64
	queueErrors    int64
	precheckErrors int64
	outcomes       map[string]int64
	backlog        int
	oldest         time.Duration
	lastSuccess    time.Time
}

// NewService checks config.
func NewService(config Config) (*Service, error) {
	switch {
	case config.Queue == nil:
		return nil, errors.New("teammail: no queue")
	case config.People == nil:
		return nil, errors.New("teammail: no people reader")
	case config.Groups == nil:
		return nil, errors.New("teammail: no group reader")
	case config.Mail == nil:
		return nil, errors.New("teammail: no mailer")
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("teammail: %w", err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Service{
		queue: config.Queue, people: config.People, groups: config.Groups, mail: config.Mail,
		now: now, logger: logger, location: location, wake: make(chan struct{}, 1),
		outcomes: map[string]int64{}, coalesced: map[Enqueued]int64{},
	}, nil
}

// Notifies reports whether g is a team's Group (TeamOf).
func (s *Service) Notifies(g identity.Group) bool {
	_, ok := TeamOf(g.Path)
	return ok
}

// MembershipChanged queues the mail of change. It never fails the write that
// made it: a queue that refuses is counted and logged, without the person.
func (s *Service) MembershipChanged(ctx context.Context, change identity.MembershipChange) {
	if _, ok := TeamOf(change.Group.Path); !ok {
		return
	}
	queued := Change{
		ID:         uuid.New(),
		SubjectID:  change.UserID,
		GroupPath:  change.Group.Path,
		Action:     ActionRemoved,
		OccurredAt: s.now().UTC(),
	}
	if change.Added {
		queued.Action = ActionAdded
	}
	if actor, err := uuid.Parse(change.ActorID); err == nil {
		queued.ActorID = &actor
	}
	// The request may be gone by now; the change still happened.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), enqueueTimeout)
	defer cancel()
	result, err := s.queue.Enqueue(ctx, queued)
	if err != nil {
		s.count(func() { s.queueErrors++ })
		s.logger.Printf("team membership mail: could not queue the mail of a change (%s): %v", queued.Action, err)
		return
	}
	if result != EnqueuedQueued {
		// Nothing new waits: no count, and no pass to wake for it.
		s.count(func() { s.coalesced[result]++ })
		return
	}
	s.count(func() { s.enqueued++ })
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// PrecheckFailed counts an add whose membership could not be read before
// the write: it went on, and no mail is sent for it. The line names nobody.
func (s *Service) PrecheckFailed() {
	s.count(func() { s.precheckErrors++ })
	s.logger.Printf("team membership mail: the membership could not be read before an add; no mail is sent for it")
}

// Report counts one pass.
type Report struct {
	Sent, Skipped, Rejected, Failed, Expired int
}

func (r Report) any() bool {
	return r.Sent+r.Skipped+r.Rejected+r.Failed+r.Expired > 0
}

// Pass drops the changes past GiveUpAfter, then sends the due ones until
// none is due. A failed send waits for its backoff; its error is the
// queue's (the database's).
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	expired, err := s.queue.DropBefore(ctx, s.now().UTC().Add(-GiveUpAfter))
	if err != nil {
		return report, err
	}
	report.Expired = expired
	s.add("expired", expired)
	for range maxSends {
		changes, err := s.queue.Claim(ctx, s.now().UTC(), lease, 1)
		if err != nil {
			return report, err
		}
		if len(changes) == 0 {
			break
		}
		change := changes[0]
		outcome := s.deliver(ctx, change)
		if outcome == outcomeInterrupted {
			// Core is stopping: the change keeps its attempts and is taken
			// again once its lease runs out. Nothing failed.
			return report, ctx.Err()
		}
		s.add(outcome, 1)
		switch outcome {
		case "sent":
			report.Sent++
		case "rejected":
			report.Rejected++
		case "failed":
			report.Failed++
		default:
			report.Skipped++
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		if outcome == "failed" {
			err = s.queue.Retry(writeCtx, change, s.now().UTC().Add(backoff(change.Attempts+1)))
		} else {
			err = s.queue.Complete(writeCtx, change)
		}
		cancel()
		if err != nil {
			return report, err
		}
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
	}
	backlog, oldest, err := s.queue.Backlog(ctx, s.now().UTC())
	if err != nil {
		return report, err
	}
	s.count(func() { s.backlog, s.oldest = backlog, oldest })
	return report, nil
}

// outcomeInterrupted is a send cut short by core stopping: no outcome.
const outcomeInterrupted = "interrupted"

// deliver sends the mail of one change and names the outcome: sent,
// skipped_<reason>, rejected (SkyMail refused the body itself), failed
// (tried again later) or interrupted (core is stopping).
func (s *Service) deliver(parent context.Context, change Change) string {
	ctx, cancel := context.WithTimeout(parent, sendTimeout)
	defer cancel()
	team, ok := TeamOf(change.GroupPath)
	if !ok {
		return "skipped_" + string(SkipNotTeam)
	}
	recipient, skip, err := s.people.Recipient(ctx, change.SubjectID)
	if err != nil {
		if parent.Err() != nil {
			return outcomeInterrupted
		}
		return "failed"
	}
	if skip != "" {
		return "skipped_" + string(skip)
	}
	group, err := s.groups.GetGroup(ctx, team.Path)
	if err != nil {
		// Renamed or gone since: the name it had stands for it.
		group = identity.Group{Path: team.Path}
	}
	leader := ""
	if change.ActorID != nil {
		leader = s.people.DisplayName(ctx, *change.ActorID)
	}
	err = s.mail.TeamMembership(ctx, recipient.Email, recipient.FullName, map[string]string{
		"TeamName":    teamName(group),
		"Action":      string(change.Action),
		"Role":        roleValues[team.Role],
		"EffectiveAt": change.OccurredAt.In(s.location).Format(dateLayout),
		"LeaderName":  leader,
	})
	var refused *mail.SendError
	switch {
	case err == nil:
		s.count(func() { s.lastSuccess = s.now().UTC() })
		return "sent"
	case errors.As(err, &refused) && refused.Permanent():
		return "rejected"
	case parent.Err() != nil:
		return outcomeInterrupted
	default:
		return "failed"
	}
}

// backoff is the wait after the nth failure in a row.
func backoff(failures int) time.Duration {
	wait := RetryFirst
	for i := 1; i < failures && wait < retryMax; i++ {
		wait *= 2
	}
	return min(wait, retryMax)
}

func (s *Service) count(update func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update()
}

func (s *Service) add(outcome string, n int) {
	if n == 0 {
		return
	}
	s.count(func() { s.outcomes[outcome] += int64(n) })
}

// Run makes a pass at once, whenever a change is queued, and every
// pollInterval. logf hears of each pass that did anything, by counts alone.
// The returned channel closes after ctx is cancelled.
func (s *Service) Run(ctx context.Context, logf func(string, ...any)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			report, err := s.Pass(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				logf("team membership mail: queue: %v", err)
			case report.any():
				logf("team membership mail: %d sent, %d skipped, %d refused by SkyMail, %d failed and retried later, %d expired",
					report.Sent, report.Skipped, report.Rejected, report.Failed, report.Expired)
			}
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-ticker.C:
			}
		}
	}()
	return done
}

// outcomeNames are the outcomes /v1/metrics always lists.
var outcomeNames = []string{
	"sent", "failed", "rejected", "expired",
	"skipped_" + string(SkipInactive), "skipped_" + string(SkipNoEmail), "skipped_" + string(SkipNotTeam),
}

// Prometheus is the mail's counters and its backlog at the last pass, in
// Prometheus' text format. No person, address or team appears.
func (s *Service) Prometheus() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out strings.Builder
	out.WriteString(enabledMetric(true))
	out.WriteString("# TYPE skylab_team_membership_mail_total counter\n")
	for _, outcome := range outcomeNames {
		fmt.Fprintf(&out, "skylab_team_membership_mail_total{outcome=%q} %d\n", outcome, s.outcomes[outcome])
	}
	fmt.Fprintf(&out, "# TYPE skylab_team_membership_mail_enqueued_total counter\nskylab_team_membership_mail_enqueued_total %d\n", s.enqueued)
	out.WriteString("# TYPE skylab_team_membership_mail_coalesced_total counter\n")
	for _, kind := range []Enqueued{EnqueuedDuplicate, EnqueuedCancelled} {
		fmt.Fprintf(&out, "skylab_team_membership_mail_coalesced_total{kind=%q} %d\n", kind, s.coalesced[kind])
	}
	fmt.Fprintf(&out, "# TYPE skylab_team_membership_mail_queue_errors_total counter\nskylab_team_membership_mail_queue_errors_total %d\n", s.queueErrors)
	fmt.Fprintf(&out, "# TYPE skylab_team_membership_mail_precheck_errors_total counter\nskylab_team_membership_mail_precheck_errors_total %d\n", s.precheckErrors)
	fmt.Fprintf(&out, "# TYPE skylab_team_membership_mail_backlog gauge\nskylab_team_membership_mail_backlog %d\n", s.backlog)
	fmt.Fprintf(&out, "# TYPE skylab_team_membership_mail_oldest_age_seconds gauge\nskylab_team_membership_mail_oldest_age_seconds %d\n", int64(s.oldest/time.Second))
	var last int64
	if !s.lastSuccess.IsZero() {
		last = s.lastSuccess.Unix()
	}
	fmt.Fprintf(&out, "# TYPE skylab_team_membership_mail_last_success_timestamp_seconds gauge\nskylab_team_membership_mail_last_success_timestamp_seconds %d\n", last)
	return out.String()
}

// Off is /v1/metrics for a core whose team membership mail is off.
type Off struct{}

func (Off) Prometheus() string { return enabledMetric(false) }

func enabledMetric(on bool) string {
	value := 0
	if on {
		value = 1
	}
	return fmt.Sprintf("# TYPE skylab_team_membership_mail_enabled gauge\nskylab_team_membership_mail_enabled %d\n", value)
}

var _ identity.MembershipNotifier = (*Service)(nil)
