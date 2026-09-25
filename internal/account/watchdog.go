package account

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Attention reasons.
const (
	AttentionOverdue            = "overdue"
	AttentionManualIntervention = "manual_intervention"
)

// DefaultWatchdogInterval is how often the watchdog counts.
const DefaultWatchdogInterval = 5 * time.Minute

// knownSteps lets an attention event name the step from a stored error code.
var knownSteps = []user.DeletionStep{
	user.DeletionStepDisableIdentity, user.DeletionStepLogoutSessions,
	user.DeletionStepEraseSkyMail, user.DeletionStepEraseCMS, user.DeletionStepEraseForms,
	user.DeletionStepAnonymizeCore, user.DeletionStepEraseProfile, user.DeletionStepEraseUploads,
	user.DeletionStepDeleteIdentity,
}

// Attention is one `account_erasure_attention` event: a request crossed the
// overdue threshold or went to manual intervention. It carries no subject.
type Attention struct {
	RequestID uuid.UUID
	Reason    string
	Step      user.DeletionStep
	Code      string
}

// LogAttention writes each event as one fixed-format line.
func LogAttention(logger *log.Logger) func(Attention) {
	return func(event Attention) {
		step, code := string(event.Step), event.Code
		if step == "" {
			step = "-"
		}
		if code == "" {
			code = "-"
		}
		logger.Printf("account_erasure_attention request_id=%s reason=%s step=%s code=%s", event.RequestID, event.Reason, step, code)
	}
}

// ErasureGauges are the watchdog's four unlabelled gauges. Nothing is
// rendered until the first successful count, so a watchdog that cannot read
// the database never reports a reassuring zero.
type ErasureGauges struct {
	mu        sync.Mutex
	counted   bool
	open      int
	overdue   int
	manual    int
	oldestAge time.Duration
}

func NewErasureGauges() *ErasureGauges { return &ErasureGauges{} }

func (g *ErasureGauges) set(open, overdue, manual int, oldestAge time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.counted, g.open, g.overdue, g.manual, g.oldestAge = true, open, overdue, manual, oldestAge
}

// Prometheus renders the gauges in the text exposition format.
func (g *ErasureGauges) Prometheus() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.counted {
		return ""
	}
	var out strings.Builder
	for _, metric := range []struct {
		name  string
		value int64
	}{
		{"skylab_account_erasure_open_requests", int64(g.open)},
		{"skylab_account_erasure_overdue_requests", int64(g.overdue)},
		{"skylab_account_erasure_manual_intervention_requests", int64(g.manual)},
		{"skylab_account_erasure_oldest_open_age_seconds", int64(g.oldestAge / time.Second)},
	} {
		out.WriteString("# TYPE " + metric.name + " gauge\n")
		out.WriteString(metric.name + " " + strconv.FormatInt(metric.value, 10) + "\n")
	}
	return out.String()
}

// WatchdogStore lists the requests that are not completed.
type WatchdogStore interface {
	OpenDeletionRequests(context.Context) ([]user.DeletionRequest, error)
}

type WatchdogConfig struct {
	// AlertAfter is when an open request is overdue, counted from its creation.
	AlertAfter time.Duration
	Now        func() time.Time
	// Attention receives each event once while its state lasts in this process.
	Attention func(Attention)
}

// Watchdog counts open, overdue and manual-intervention requests for the
// gauges and announces each request that needs a person.
type Watchdog struct {
	store     WatchdogStore
	gauges    *ErasureGauges
	config    WatchdogConfig
	announced map[attentionKey]bool
}

type attentionKey struct {
	request uuid.UUID
	reason  string
}

func NewWatchdog(store WatchdogStore, gauges *ErasureGauges, config WatchdogConfig) *Watchdog {
	if config.AlertAfter <= 0 {
		config.AlertAfter = 480 * time.Hour
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.Attention == nil {
		config.Attention = LogAttention(log.Default())
	}
	return &Watchdog{store: store, gauges: gauges, config: config, announced: map[attentionKey]bool{}}
}

// RunOnce counts once. A failed count leaves the last published values.
func (w *Watchdog) RunOnce(ctx context.Context) error {
	requests, err := w.store.OpenDeletionRequests(ctx)
	if err != nil {
		return err
	}
	now := w.config.Now()
	var overdue, manual int
	var oldest time.Time
	current := make(map[attentionKey]bool)
	for _, request := range requests {
		if request.Status == user.DeletionRequestCompleted {
			continue
		}
		if oldest.IsZero() || request.CreatedAt.Before(oldest) {
			oldest = request.CreatedAt
		}
		if request.Status == user.DeletionRequestManualIntervention {
			manual++
			w.announce(current, request, AttentionManualIntervention)
		}
		if !now.Before(request.CreatedAt.Add(w.config.AlertAfter)) {
			overdue++
			w.announce(current, request, AttentionOverdue)
		}
	}
	w.announced = current
	var oldestAge time.Duration
	if !oldest.IsZero() && now.After(oldest) {
		oldestAge = now.Sub(oldest)
	}
	w.gauges.set(len(requests), overdue, manual, oldestAge)
	return nil
}

func (w *Watchdog) announce(current map[attentionKey]bool, request user.DeletionRequest, reason string) {
	key := attentionKey{request: request.ID, reason: reason}
	current[key] = true
	if w.announced[key] {
		return
	}
	w.config.Attention(Attention{RequestID: request.ID, Reason: reason, Step: stepOf(request.LastErrorCode), Code: request.LastErrorCode})
}

// stepOf names the step a stored error code belongs to, or "" when the code
// is not a step's (for example platform_block_failed).
func stepOf(code string) user.DeletionStep {
	var match user.DeletionStep
	for _, step := range knownSteps {
		if strings.HasPrefix(code, string(step)+"_") && len(step) > len(match) {
			match = step
		}
	}
	return match
}

// MaintainWatchdog counts now and then every interval until ctx ends.
func MaintainWatchdog(ctx context.Context, watchdog *Watchdog, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = DefaultWatchdogInterval
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if err := watchdog.RunOnce(ctx); err != nil && onError != nil {
				onError(err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
