package githubactivity

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"golang.org/x/sync/singleflight"
)

var (
	// ErrForbidden is a caller who may not read the activity.
	ErrForbidden = errors.New("githubactivity: forbidden")
	// ErrUnavailable is no activity to answer with: the settings are wrong,
	// or GitHub could not be read and nothing was read before.
	ErrUnavailable = errors.New("githubactivity: unavailable")
)

const (
	// DefaultTTL is how long an answer is served before GitHub is read again.
	DefaultTTL = 10 * time.Minute
	// DefaultRetryAfterFailure is how long after a failed read GitHub is left
	// alone; the last good answer (or 503) is served meanwhile.
	DefaultRetryAfterFailure = time.Minute
	// DefaultRefreshTimeout bounds one read of GitHub.
	DefaultRefreshTimeout = 45 * time.Second
)

// Options tune a Service. Zero values take the defaults.
type Options struct {
	// APIURL is GitHub's API; tests point it at a fake.
	APIURL            string
	HTTP              *http.Client
	TTL               time.Duration
	RetryAfterFailure time.Duration
	RefreshTimeout    time.Duration
	Now               func() time.Time
	// Logf receives refresh failures; they name paths and statuses, never a
	// token. Nil uses log.Printf.
	Logf func(format string, args ...any)
}

// Service answers the activity to the people allowed to read it, from a
// ten-minute cache. One read of GitHub runs at a time, whoever asks; a failed
// read serves the last good answer marked stale.
type Service struct {
	az         authz.Authorizer
	collect    func(context.Context) (Activity, error)
	broken     error
	ttl        time.Duration
	retryAfter time.Duration
	timeout    time.Duration
	now        func() time.Time
	logf       func(string, ...any)
	refreshes  singleflight.Group

	mu       sync.Mutex
	last     *Activity
	lastAt   time.Time
	failedAt time.Time
}

// New is the Service for config.
func New(config Config, az authz.Authorizer, options Options) *Service {
	s := newService(az, options)
	gh := newClient(config, options.APIURL, options.HTTP, s.now)
	c := &collector{gh: gh, org: config.Org, days: config.WindowDays, now: s.now}
	if c.days <= 0 {
		c.days = DefaultWindowDays
	}
	s.collect = c.collect
	return s
}

// Unavailable is the Service of settings that are set but wrong: it checks
// the caller as usual and then answers ErrUnavailable, so the route answers
// 503 instead of disappearing.
func Unavailable(az authz.Authorizer, reason error) *Service {
	s := newService(az, Options{})
	s.broken = reason
	return s
}

func newService(az authz.Authorizer, options Options) *Service {
	s := &Service{
		az: az, ttl: options.TTL, retryAfter: options.RetryAfterFailure, timeout: options.RefreshTimeout,
		now: options.Now, logf: options.Logf,
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	if s.retryAfter <= 0 {
		s.retryAfter = DefaultRetryAfterFailure
	}
	if s.timeout <= 0 {
		s.timeout = DefaultRefreshTimeout
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	return s
}

// Allowed reports whether p may read the activity: a person in a privileged
// Group (ADMIN, YK, DK), never a product's service account. It is internal
// club data, and the private repositories' totals are not public.
func (s *Service) Allowed(p authz.Principal) bool {
	return s.az.Allow(p, authz.Resource{Type: authz.TypeGithubActivity}, authz.Read)
}

// Get is the activity for p.
func (s *Service) Get(ctx context.Context, p authz.Principal) (Activity, error) {
	if !s.Allowed(p) {
		return Activity{}, ErrForbidden
	}
	if s.broken != nil {
		return Activity{}, ErrUnavailable
	}
	s.mu.Lock()
	now := s.now()
	if s.last != nil && now.Sub(s.lastAt) < s.ttl {
		answer := *s.last
		s.mu.Unlock()
		return answer, nil
	}
	if !s.failedAt.IsZero() && now.Sub(s.failedAt) < s.retryAfter {
		answer, err := s.staleLocked()
		s.mu.Unlock()
		return answer, err
	}
	s.mu.Unlock()

	// The read is not the caller's: it runs on its own clock, so a caller
	// who gives up does not cancel it for the others waiting on it.
	result := s.refreshes.DoChan("refresh", func() (any, error) { return s.refresh() })
	select {
	case r := <-result:
		if r.Err != nil {
			return s.stale()
		}
		return r.Val.(Activity), nil
	case <-ctx.Done():
		return s.stale()
	}
}

func (s *Service) refresh() (activity Activity, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	// singleflight runs this in a goroutine of its own: a panic here would
	// end the process.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("githubactivity: refresh panicked: %v", r)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.failedAt = s.now()
			s.logf("github activity: refresh failed: %v", err)
			return
		}
		s.last, s.lastAt, s.failedAt = &activity, s.now(), time.Time{}
	}()
	return s.collect(ctx)
}

func (s *Service) stale() (Activity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staleLocked()
}

func (s *Service) staleLocked() (Activity, error) {
	if s.last == nil {
		return Activity{}, ErrUnavailable
	}
	return s.last.stale(), nil
}
