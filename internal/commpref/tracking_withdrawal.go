package commpref

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The withdrawal tells SkyMail that a person turned tracking off (hub ticket
// 16, SkyMail's ticket 14c): SkyMail closes every tracking token of their
// mail and deletes their openings and clicks; their old mail's links still
// redirect. One way, core → SkyMail, through the outbox Apply writes:
//
//	PUT {WithdrawalURLEnv}/internal/v1/tracking-withdrawals/{sub}
//	Authorization: Bearer <core's service account token>
//
// with no body. SkyMail wants azp core, aud ∋ skymail and SkyMail's client
// role WithdrawRole; core-erasure's erase roles are not enough.

// WithdrawRole is SkyMail's client role core's service account (client
// core) needs for the withdrawal. It is SkyMail's: core only names it.
const WithdrawRole = "skymail:tracking:withdraw"

const (
	// WithdrawalURLEnv is SkyMail's address on the internal network (the
	// public one answers /internal 404). Unset: ACCOUNT_ERASURE_SKYMAIL_URL,
	// the same address the Erasure command uses.
	WithdrawalURLEnv = "COMMUNICATION_TRACKING_WITHDRAWAL_URL"
	// withdrawalURLFallbackEnv is the Erasure command's SkyMail address.
	withdrawalURLFallbackEnv = "ACCOUNT_ERASURE_SKYMAIL_URL"
)

// WithdrawalURLFromEnv reads SkyMail's internal address and names the
// variable it came from; "" when neither is set. An error names the
// variable, never its value.
func WithdrawalURLFromEnv(getenv func(string) string) (base, from string, err error) {
	for _, name := range []string{WithdrawalURLEnv, withdrawalURLFallbackEnv} {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
			return "", name, fmt.Errorf("%s must be an absolute http or https base URL without credentials, query or fragment", name)
		}
		return strings.TrimRight(raw, "/"), name, nil
	}
	return "", "", nil
}

// WithdrawalOutcome is how SkyMail answered one withdrawal.
type WithdrawalOutcome string

const (
	// WithdrawalSent: 204, done (a repeat changes nothing).
	WithdrawalSent WithdrawalOutcome = "sent"
	// WithdrawalRetry: no answer, a token that did not come, 401, 429 or
	// 5xx (503 with Retry-After): tried again.
	WithdrawalRetry WithdrawalOutcome = "retry"
	// WithdrawalForbidden: 403, a configuration error (core's service
	// account lacks WithdrawRole or aud ∋ skymail). Alarmed and tried again
	// slowly: the realm's fix makes it go.
	WithdrawalForbidden WithdrawalOutcome = "forbidden"
	// WithdrawalNotFound: 404, a SkyMail without the route yet (core ships
	// first) or a public address. Tried again slowly.
	WithdrawalNotFound WithdrawalOutcome = "not_found"
	// WithdrawalRejected: 400, SkyMail refuses the subject for good. The
	// withdrawal is dropped and alarmed.
	WithdrawalRejected WithdrawalOutcome = "rejected"
)

// withdrawalOutcomes are the outcomes /v1/metrics always lists.
var withdrawalOutcomes = []WithdrawalOutcome{WithdrawalSent, WithdrawalRetry, WithdrawalForbidden, WithdrawalNotFound, WithdrawalRejected}

// WithdrawalResult is one send's outcome.
type WithdrawalResult struct {
	Outcome WithdrawalOutcome
	// Status is the HTTP status, 0 without an answer.
	Status int
	// RetryAfter is SkyMail's Retry-After, 0 without one.
	RetryAfter time.Duration
	// Reason says what failed, never naming the person, the token or the
	// address.
	Reason string
}

// WithdrawalSender tells SkyMail of one withdrawal.
type WithdrawalSender interface {
	Withdraw(ctx context.Context, subject uuid.UUID) WithdrawalResult
}

// TokenSource hands out core's service account token for SkyMail and
// forgets it when SkyMail refuses it.
type TokenSource interface {
	Token(context.Context) (string, error)
	Invalidate()
}

// withdrawalCallTimeout bounds one call; withdrawalMaxBody is the most of an
// answer core reads (it reads none of it).
const (
	withdrawalCallTimeout = 15 * time.Second
	withdrawalMaxBody     = 16 << 10
)

// SkyMailWithdrawals is the HTTP sender.
type SkyMailWithdrawals struct {
	// BaseURL is SkyMail's internal address (WithdrawalURLFromEnv).
	BaseURL string
	Tokens  TokenSource
	HTTP    *http.Client
}

// directTransport never goes through an HTTP proxy from the environment:
// the call is for the internal network only.
var directTransport = func() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

func (c *SkyMailWithdrawals) httpClient() *http.Client {
	client := http.Client{Timeout: withdrawalCallTimeout, Transport: directTransport}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	// A redirect would carry the token somewhere the address did not name.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// Withdraw sends one withdrawal and classifies the answer.
func (c *SkyMailWithdrawals) Withdraw(ctx context.Context, subject uuid.UUID) WithdrawalResult {
	if subject == uuid.Nil {
		return WithdrawalResult{Outcome: WithdrawalRejected, Reason: "no subject"}
	}
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return WithdrawalResult{Outcome: WithdrawalRetry, Reason: "token unavailable"}
	}
	// The subject is a lower-case hyphenated UUID, as SkyMail stores it.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+"/internal/v1/tracking-withdrawals/"+subject.String(), http.NoBody)
	if err != nil {
		return WithdrawalResult{Outcome: WithdrawalRejected, Reason: "request could not be built"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return WithdrawalResult{Outcome: WithdrawalRetry, Reason: "request failed (" + transportKind(err) + ")"}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, withdrawalMaxBody))
	result := WithdrawalResult{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	switch code := resp.StatusCode; {
	case code == http.StatusNoContent || code == http.StatusOK:
		result.Outcome = WithdrawalSent
	case code == http.StatusUnauthorized:
		c.Tokens.Invalidate()
		result.Outcome, result.Reason = WithdrawalRetry, "token rejected (401)"
	case code == http.StatusForbidden:
		// The fix (the role given, the audience mapped) reaches only a new
		// token.
		c.Tokens.Invalidate()
		result.Outcome, result.Reason = WithdrawalForbidden, "forbidden (403)"
	case code == http.StatusNotFound:
		result.Outcome, result.Reason = WithdrawalNotFound, "no such route (404)"
	case code == http.StatusBadRequest:
		result.Outcome, result.Reason = WithdrawalRejected, "subject refused (400)"
	default:
		result.Outcome, result.Reason = WithdrawalRetry, fmt.Sprintf("unavailable (%d)", code)
	}
	return result
}

// retryAfter reads Retry-After (seconds or an HTTP date); 0 without a usable
// value.
func retryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(math.MaxInt64/int64(time.Second)) {
			return withdrawalRetryMax
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}

// transportKind names a transport failure without its text, which carries
// the address.
func transportKind(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "connection failed"
	}
}

const (
	// withdrawalRetryFirst is the wait after a first failure; it doubles
	// with each failure in a row up to withdrawalRetryMax. A Retry-After is
	// kept between the two. A 403 or 404 waits withdrawalRetryMax. A
	// withdrawal is never given up on: the person's objection must reach
	// SkyMail, and the backlog gauge alarms meanwhile.
	withdrawalRetryFirst = 30 * time.Second
	withdrawalRetryMax   = 15 * time.Minute
	// withdrawalLease is how long a pass holds a claimed withdrawal; it
	// outlasts one call and its token request.
	withdrawalLease = 2 * time.Minute
	// withdrawalPollInterval is how often the worker looks at the queue
	// when no write wakes it.
	withdrawalPollInterval = 15 * time.Second
	// withdrawalMaxSends bounds one pass.
	withdrawalMaxSends = 200
	// withdrawalWriteTimeout bounds the row's own update after a send, which
	// runs even when core is stopping.
	withdrawalWriteTimeout = 5 * time.Second
)

// withdrawalWait is how long a withdrawal waits after its nth failure in a
// row.
func withdrawalWait(result WithdrawalResult, failures int) time.Duration {
	switch result.Outcome {
	case WithdrawalForbidden, WithdrawalNotFound:
		return withdrawalRetryMax
	}
	if result.RetryAfter > 0 {
		return min(max(result.RetryAfter, withdrawalRetryFirst), withdrawalRetryMax)
	}
	wait := withdrawalRetryFirst
	for i := 1; i < failures && wait < withdrawalRetryMax; i++ {
		wait *= 2
	}
	return min(wait, withdrawalRetryMax)
}

// WithdrawalReport is what one pass did.
type WithdrawalReport struct {
	Sent, Retried, Forbidden, NotFound, Rejected int
}

func (r WithdrawalReport) any() bool {
	return r.Sent+r.Retried+r.Forbidden+r.NotFound+r.Rejected > 0
}

// Withdrawals is the worker that sends the queued withdrawals. Without a
// sender (no SkyMail address or no client credentials) it sends nothing and
// only keeps the backlog gauges.
type Withdrawals struct {
	queue  WithdrawalQueue
	sender WithdrawalSender
	now    func() time.Time
	logf   func(string, ...any)
	wake   chan struct{}

	mu          sync.Mutex
	outcomes    map[WithdrawalOutcome]int64
	queueErrors int64
	backlog     int
	oldest      time.Duration
	lastSuccess time.Time
}

// NewWithdrawals sends queue's withdrawals with sender (nil: none).
func NewWithdrawals(queue WithdrawalQueue, sender WithdrawalSender) *Withdrawals {
	return &Withdrawals{
		queue: queue, sender: sender, now: time.Now, logf: func(string, ...any) {},
		wake: make(chan struct{}, 1), outcomes: map[WithdrawalOutcome]int64{},
	}
}

// SetClock replaces the clock (tests).
func (w *Withdrawals) SetClock(now func() time.Time) { w.now = now }

// Sending reports whether the worker has a sender.
func (w *Withdrawals) Sending() bool { return w != nil && w.sender != nil }

// Wake makes the worker look at the queue now.
func (w *Withdrawals) Wake() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Pass sends the due withdrawals until none is due, then reads the backlog.
// Its error is the queue's (the database's).
func (w *Withdrawals) Pass(ctx context.Context) (WithdrawalReport, error) {
	var report WithdrawalReport
	for range withdrawalMaxSends {
		if w.sender == nil {
			break
		}
		claimed, err := w.queue.ClaimWithdrawals(ctx, w.now().UTC(), withdrawalLease, 1)
		if err != nil {
			w.count(func() { w.queueErrors++ })
			return report, err
		}
		if len(claimed) == 0 {
			break
		}
		item := claimed[0]
		result := w.sender.Withdraw(ctx, item.Subject)
		if ctx.Err() != nil && result.Outcome != WithdrawalSent {
			// Core is stopping: the withdrawal is taken again once its
			// lease runs out. Nothing failed.
			return report, ctx.Err()
		}
		w.count(func() {
			w.outcomes[result.Outcome]++
			if result.Outcome == WithdrawalSent {
				w.lastSuccess = w.now().UTC()
			}
		})
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), withdrawalWriteTimeout)
		switch result.Outcome {
		case WithdrawalSent:
			report.Sent++
			err = w.queue.CompleteWithdrawal(writeCtx, item)
		case WithdrawalRejected:
			report.Rejected++
			w.logf(`{"event":"tracking_withdrawal_rejected","status":%d,"attempts":%d,"reason":%q}`, result.Status, item.Attempts+1, result.Reason)
			err = w.queue.CompleteWithdrawal(writeCtx, item)
		default:
			switch result.Outcome {
			case WithdrawalForbidden:
				report.Forbidden++
				w.logf(`{"event":"tracking_withdrawal_forbidden","status":%d,"attempts":%d,"reason":"core's service account needs %s and aud skymail"}`, result.Status, item.Attempts+1, WithdrawRole)
			case WithdrawalNotFound:
				report.NotFound++
				w.logf(`{"event":"tracking_withdrawal_not_found","status":%d,"attempts":%d,"reason":%q}`, result.Status, item.Attempts+1, result.Reason)
			default:
				report.Retried++
				w.logf(`{"event":"tracking_withdrawal_retry","status":%d,"attempts":%d,"reason":%q}`, result.Status, item.Attempts+1, result.Reason)
			}
			err = w.queue.RetryWithdrawal(writeCtx, item, w.now().UTC().Add(withdrawalWait(result, item.Attempts+1)), result.Status)
		}
		cancel()
		if err != nil {
			w.count(func() { w.queueErrors++ })
			return report, err
		}
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
	}
	backlog, oldest, err := w.queue.WithdrawalBacklog(ctx, w.now().UTC())
	if err != nil {
		w.count(func() { w.queueErrors++ })
		return report, err
	}
	w.count(func() { w.backlog, w.oldest = backlog, oldest })
	return report, nil
}

func (w *Withdrawals) count(update func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	update()
}

// Run makes a pass at once, whenever a write wakes it, and every
// withdrawalPollInterval. logf hears of each pass that did anything, by
// counts alone, and of every failed send by its status. The returned
// channel closes after ctx is cancelled.
func (w *Withdrawals) Run(ctx context.Context, logf func(string, ...any)) <-chan struct{} {
	if logf != nil {
		w.logf = logf
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(withdrawalPollInterval)
		defer ticker.Stop()
		for {
			report, err := w.Pass(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				w.logf("tracking withdrawal: queue: %v", err)
			case report.any():
				w.logf(`{"event":"tracking_withdrawal_pass","sent":%d,"retried":%d,"forbidden":%d,"not_found":%d,"rejected":%d}`,
					report.Sent, report.Retried, report.Forbidden, report.NotFound, report.Rejected)
			}
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
			case <-ticker.C:
			}
		}
	}()
	return done
}

// Prometheus is the withdrawals' counters and backlog, in Prometheus' text
// format. No person appears.
func (w *Withdrawals) Prometheus() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out strings.Builder
	enabled := 0
	if w.sender != nil {
		enabled = 1
	}
	fmt.Fprintf(&out, "# TYPE skylab_tracking_withdrawal_enabled gauge\nskylab_tracking_withdrawal_enabled %d\n", enabled)
	out.WriteString("# TYPE skylab_tracking_withdrawal_total counter\n")
	for _, outcome := range withdrawalOutcomes {
		fmt.Fprintf(&out, "skylab_tracking_withdrawal_total{outcome=%q} %d\n", outcome, w.outcomes[outcome])
	}
	fmt.Fprintf(&out, "# TYPE skylab_tracking_withdrawal_queue_errors_total counter\nskylab_tracking_withdrawal_queue_errors_total %d\n", w.queueErrors)
	fmt.Fprintf(&out, "# TYPE skylab_tracking_withdrawal_backlog gauge\nskylab_tracking_withdrawal_backlog %d\n", w.backlog)
	fmt.Fprintf(&out, "# TYPE skylab_tracking_withdrawal_oldest_age_seconds gauge\nskylab_tracking_withdrawal_oldest_age_seconds %d\n", int64(w.oldest/time.Second))
	var last int64
	if !w.lastSuccess.IsZero() {
		last = w.lastSuccess.Unix()
	}
	fmt.Fprintf(&out, "# TYPE skylab_tracking_withdrawal_last_success_timestamp_seconds gauge\nskylab_tracking_withdrawal_last_success_timestamp_seconds %d\n", last)
	return out.String()
}

// TrackingStartupLines are the startup log lines of tracking: the mode, and
// whether withdrawals reach SkyMail (and from which variable its address
// came).
func TrackingStartupLines(mode TrackingMode, sending bool, urlFrom string) []string {
	lines := []string{fmt.Sprintf("communication tracking: mode %s (%s)", mode, TrackingModeEnv)}
	if sending {
		lines = append(lines, fmt.Sprintf("communication tracking: withdrawals to SkyMail on (address from %s)", urlFrom))
	} else {
		lines = append(lines, fmt.Sprintf("WARNING communication tracking: withdrawals to SkyMail off (needs %s or %s, KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID and KEYCLOAK_CLIENT_SECRET); a person who turns tracking off waits in the queue",
			WithdrawalURLEnv, withdrawalURLFallbackEnv))
	}
	return lines
}
