package erasure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// CommandTimeout bounds one HTTP call; a service answers within 10 seconds
	// or commits what it did and says 202 (spec §2.6).
	CommandTimeout = 15 * time.Second
	// MaxAddresses is the School, Personal and Primary e-mail union.
	MaxAddresses = 3
	// MaxAddressLength is the longest address a command carries.
	MaxAddressLength = 254
	// MaxCountKeys bounds the counts a service reports.
	MaxCountKeys = 32

	minRetryAfter     = 30 * time.Second
	maxRetryAfter     = 15 * time.Minute
	defaultRetryAfter = 5 * time.Minute
	maxResponseBody   = 16 << 10
)

// countKey keeps a counts key snake_case, which also keeps an address or a
// name out of the durable proof.
var countKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Command is one Erasure command. Emails live only for the length of the call.
type Command struct {
	RequestID uuid.UUID
	SubjectID uuid.UUID
	Emails    []string
}

// Result is a service's completion answer.
type Result struct {
	Counts map[string]int64
}

// TokenSource hands out the bearer for one service's erase scope.
type TokenSource interface {
	Token(context.Context) (string, error)
	Invalidate()
}

// Client sends the Erasure command to one service.
type Client struct {
	Service Service
	BaseURL string
	Tokens  TokenSource
	HTTP    *http.Client
	Now     func() time.Time
}

// NewClient builds the client for one configured endpoint with its own token
// cache for that service's scope.
func NewClient(endpoint Endpoint, tokenURL, clientID, clientSecret string) *Client {
	return &Client{
		Service: endpoint.Service,
		BaseURL: endpoint.BaseURL,
		Tokens: &ClientCredentials{
			TokenURL: tokenURL, ClientID: clientID, ClientSecret: clientSecret, Scope: endpoint.Service.Scope,
		},
	}
}

// DeferredError says the service is busy or unreachable: try the same command
// again at RetryAt without spending an attempt.
type DeferredError struct {
	Step   user.DeletionStep
	Reason string
	At     time.Time
}

func (e *DeferredError) Error() string      { return string(e.Step) + ": " + e.Reason }
func (e *DeferredError) RetryAt() time.Time { return e.At }

// RejectedError is a permanent configuration or contract failure. The request
// goes to manual intervention under PermanentCode.
type RejectedError struct {
	Step   user.DeletionStep
	Status int
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("%s: rejected with status %d", e.Step, e.Status)
}

// PermanentCode is `erase_<service>_rejected_<http>`.
func (e *RejectedError) PermanentCode() string {
	return fmt.Sprintf("%s_rejected_%d", e.Step, e.Status)
}

// Error is an ordinary failure: the next attempt spends one of the budget.
type Error struct {
	Step   user.DeletionStep
	Reason string
}

func (e *Error) Error() string { return string(e.Step) + ": " + e.Reason }

// Erase sends the command and classifies the answer (spec §2.4). No error it
// returns carries the subject, an address or anything the service wrote.
func (c *Client) Erase(ctx context.Context, command Command) (Result, error) {
	emails, err := NormalizeEmails(command.Emails)
	if err != nil {
		return Result{}, &Error{Step: c.Service.Step, Reason: err.Error()}
	}
	if command.RequestID == uuid.Nil || command.SubjectID == uuid.Nil {
		return Result{}, &Error{Step: c.Service.Step, Reason: "command without a request or subject id"}
	}
	body, err := json.Marshal(struct {
		RequestID uuid.UUID `json:"request_id"`
		SubjectID uuid.UUID `json:"subject_id"`
		Emails    []string  `json:"emails"`
	}{command.RequestID, command.SubjectID, emails})
	if err != nil {
		return Result{}, &Error{Step: c.Service.Step, Reason: "command could not be encoded"}
	}

	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return Result{}, c.deferred("token unavailable: "+err.Error(), "")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+"/internal/v1/account-erasures/"+command.RequestID.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, &Error{Step: c.Service.Step, Reason: "request could not be built"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Result{}, c.deferred("request failed ("+transportKind(err)+")", "")
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))

	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		if readErr != nil {
			return Result{}, c.deferred("completion body could not be read", "")
		}
		counts, err := completion(raw, command.RequestID)
		if err != nil {
			return Result{}, &Error{Step: c.Service.Step, Reason: "invalid completion body: " + err.Error()}
		}
		return Result{Counts: counts}, nil
	case code == http.StatusAccepted:
		return Result{}, c.deferred("in progress (202)", resp.Header.Get("Retry-After"))
	case code == http.StatusTooManyRequests || code == http.StatusInternalServerError ||
		code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout:
		return Result{}, c.deferred(fmt.Sprintf("service unavailable (%d)", code), resp.Header.Get("Retry-After"))
	case code == http.StatusUnauthorized:
		c.Tokens.Invalidate()
		return Result{}, &Error{Step: c.Service.Step, Reason: "token rejected (401)"}
	case code == http.StatusBadRequest || code == http.StatusForbidden || code == http.StatusNotFound || code == http.StatusConflict:
		return Result{}, &RejectedError{Step: c.Service.Step, Status: code}
	default:
		return Result{}, &Error{Step: c.Service.Step, Reason: fmt.Sprintf("unexpected status %d", code)}
	}
}

func (c *Client) deferred(reason, retryAfter string) error {
	now := c.now()
	return &DeferredError{Step: c.Service.Step, Reason: reason, At: now.Add(retryDelay(retryAfter, now))}
}

// directTransport never goes through an HTTP proxy from the environment: the
// command body carries addresses and is meant for the internal network only.
var directTransport = func() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

func (c *Client) httpClient() *http.Client {
	client := http.Client{Timeout: CommandTimeout, Transport: directTransport}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	// A redirect would carry the body, and its addresses, somewhere the
	// registry did not name.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// NormalizeEmails trims and lower-cases the addresses and drops blanks and
// repeats. More than three, or one longer than 254 characters, is refused; the
// error names neither.
func NormalizeEmails(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" || seen[email] {
			continue
		}
		if len(email) > MaxAddressLength {
			return nil, fmt.Errorf("an address is longer than %d characters", MaxAddressLength)
		}
		seen[email] = true
		out = append(out, email)
	}
	if len(out) > MaxAddresses {
		return nil, fmt.Errorf("more than %d addresses", MaxAddresses)
	}
	return out, nil
}

func completion(raw []byte, requestID uuid.UUID) (map[string]int64, error) {
	if len(raw) > maxResponseBody {
		return nil, errors.New("too large")
	}
	var body struct {
		RequestID string          `json:"request_id"`
		Status    string          `json:"status"`
		Counts    json.RawMessage `json:"counts"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("not a JSON object")
	}
	if body.Status != "completed" {
		return nil, errors.New("status is not completed")
	}
	if id, err := uuid.Parse(body.RequestID); err != nil || id != requestID {
		return nil, errors.New("request_id does not match")
	}
	return parseCounts(body.Counts)
}

func parseCounts(raw json.RawMessage) (map[string]int64, error) {
	var values map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, errors.New("counts is not an object")
	}
	if len(values) > MaxCountKeys {
		return nil, fmt.Errorf("counts has more than %d keys", MaxCountKeys)
	}
	counts := make(map[string]int64, len(values))
	for key, value := range values {
		if !countKey.MatchString(key) {
			return nil, errors.New("a counts key is not snake_case")
		}
		n, err := strconv.ParseInt(string(bytes.TrimSpace(value)), 10, 64)
		if err != nil || n < 0 {
			return nil, errors.New("a counts value is not a non-negative integer")
		}
		counts[key] = n
	}
	return counts, nil
}

// retryDelay reads Retry-After (seconds or an HTTP date) and keeps it between
// 30 seconds and 15 minutes; without a usable value it waits 5 minutes.
func retryDelay(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return defaultRetryAfter
	}
	var wait time.Duration
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds > int64(math.MaxInt64/int64(time.Second)) {
			return maxRetryAfter
		}
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(header); err == nil {
		wait = at.Sub(now)
	} else {
		return defaultRetryAfter
	}
	return min(max(wait, minRetryAfter), maxRetryAfter)
}

// transportKind names a transport failure without its text, which carries the
// URL and, for some resolvers, more than that.
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
