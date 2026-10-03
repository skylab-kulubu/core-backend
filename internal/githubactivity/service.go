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
)

var (
	// ErrForbidden is a caller who may not read the activity.
	ErrForbidden = errors.New("githubactivity: forbidden")
	// ErrUnavailable is no activity to answer with: the settings are wrong,
	// or GitHub could not be read and nothing was read before.
	ErrUnavailable = errors.New("githubactivity: unavailable")
)

const (
	// DefaultTTL is how long an answer is served before GitHub is read again
	// (in the background, the answer still served meanwhile).
	DefaultTTL = 10 * time.Minute
	// DefaultMaxStale is how old a last good answer may grow while GitHub
	// cannot be read; past it the answer is 503.
	DefaultMaxStale = 12 * time.Hour
	// DefaultRetryAfterFailure is how long after a failed read GitHub is left
	// alone; the last good answer (or 503) is served meanwhile.
	DefaultRetryAfterFailure = time.Minute
)

// Options tune a Service. Zero values take the defaults.
type Options struct {
	// APIURL is GitHub's API; tests point it at a fake.
	APIURL            string
	HTTP              *http.Client
	TTL               time.Duration
	MaxStale          time.Duration
	RetryAfterFailure time.Duration
	// RefreshTimeout overrides Config.RefreshTimeout.
	RefreshTimeout time.Duration
	Now            func() time.Time
	// Logf receives refresh failures; they name paths and statuses (a
	// repository by a hash), never a token. Nil uses log.Printf.
	Logf func(format string, args ...any)
}

// Service answers the activity to the people allowed to read it. An answer is
// fresh for ten minutes; after that it is still served at once while GitHub is
// read again in the background, and only the very first caller (or one whose
// answer is past MaxStale) waits for GitHub. One read runs at a time, whoever
// asks. A failed read leaves the last good answer served marked stale, until
// it is twelve hours old.
type Service struct {
	az         authz.Authorizer
	collect    func(context.Context) (Activity, error)
	broken     error
	ttl        time.Duration
	maxStale   time.Duration
	retryAfter time.Duration
	timeout    time.Duration
	now        func() time.Time
	logf       func(string, ...any)

	mu       sync.Mutex
	last     *Activity
	lastAt   time.Time
	failedAt time.Time
	flight   *flight
}

// flight is the read of GitHub in progress; done closes when it is over.
type flight struct {
	done chan struct{}
	err  error
}

// New is the Service for config.
func New(config Config, az authz.Authorizer, options Options) *Service {
	if options.RefreshTimeout <= 0 {
		options.RefreshTimeout = config.RefreshTimeout
	}
	s := newService(az, options)
	gh := newClient(config, options.APIURL, options.HTTP, s.now)
	c := &collector{gh: gh, org: config.Org, days: config.WindowDays, workers: config.Workers, maxPages: config.MaxPages, now: s.now}
	if c.days <= 0 {
		c.days = DefaultWindowDays
	}
	if c.workers <= 0 {
		c.workers = DefaultWorkers
	}
	if c.maxPages <= 0 {
		c.maxPages = DefaultMaxPages
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
		az: az, ttl: options.TTL, maxStale: options.MaxStale, retryAfter: options.RetryAfterFailure,
		timeout: options.RefreshTimeout, now: options.Now, logf: options.Logf,
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	if s.maxStale <= 0 {
		s.maxStale = DefaultMaxStale
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
	if s.last != nil && now.Sub(s.lastAt) < s.maxStale {
		answer := *s.last
		// A read failed since this answer: GitHub could not be reached.
		answer.Stale = !s.failedAt.IsZero()
		if now.Sub(s.lastAt) >= s.ttl && !s.coolingLocked(now) {
			s.startLocked()
		}
		s.mu.Unlock()
		return answer, nil
	}
	if s.coolingLocked(now) {
		s.mu.Unlock()
		return Activity{}, ErrUnavailable
	}
	f := s.startLocked()
	s.mu.Unlock()

	select {
	case <-f.done:
	case <-ctx.Done():
		// The read goes on for the next caller.
		return Activity{}, ErrUnavailable
	}
	if f.err != nil {
		return Activity{}, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return Activity{}, ErrUnavailable
	}
	return *s.last, nil
}

// coolingLocked is the minute after a failed read, when GitHub is left alone.
func (s *Service) coolingLocked(now time.Time) bool {
	return !s.failedAt.IsZero() && now.Sub(s.failedAt) < s.retryAfter
}

// startLocked starts a read of GitHub unless one is in flight, and returns it.
func (s *Service) startLocked() *flight {
	if s.flight != nil {
		return s.flight
	}
	f := &flight{done: make(chan struct{})}
	s.flight = f
	go s.refresh(f)
	return f
}

// refresh reads GitHub on its own clock: no caller's context cancels it.
func (s *Service) refresh(f *flight) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	var activity Activity
	var err error
	func() {
		// The read runs in this goroutine; a panic in it (outside the
		// repository workers, which recover their own) would end the
		// process.
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("githubactivity: refresh panicked: %v", r)
			}
		}()
		activity, err = s.collect(ctx)
	}()
	s.mu.Lock()
	if err != nil {
		s.failedAt = s.now()
		s.logf("github activity: refresh failed: %v", err)
	} else {
		s.last, s.lastAt, s.failedAt = &activity, s.now(), time.Time{}
	}
	f.err = err
	s.flight = nil
	s.mu.Unlock()
	close(f.done)
}

// waitRefresh waits for the read in flight, if any (tests).
func (s *Service) waitRefresh() {
	s.mu.Lock()
	f := s.flight
	s.mu.Unlock()
	if f != nil {
		<-f.done
	}
}
