// Package health answers whether this core task should be given traffic
// (GET /v1/ready, the container's health check) and lets shutdown take it
// out of rotation before it stops.
//
// Readiness is the process and its own database, nothing else: a task that
// cannot reach PostgreSQL can answer almost nothing, while Keycloak,
// SkyMail, Gotenberg, ClamAV or R2 being down fails only the requests that
// need them, the same on every task, so taking tasks out (or letting Swarm
// restart them) for those would turn a partial outage into a whole one.
// Migrations are done by construction: core starts listening only after
// migrate.Apply has returned. The account access gate keeps its own check
// beside this one (internal/httpx), as before.
package health

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Database is what readiness pings: core's own PostgreSQL.
type Database interface {
	Ping(context.Context) error
}

const (
	// DefaultPingTimeout bounds one ping. It stays under the health
	// check's own timeout (the healthcheck command waits 3 s, Swarm 5 s).
	DefaultPingTimeout = 2 * time.Second
	// DefaultMaxAge is how long an answer is reused. /v1/ready is public,
	// so however often it is asked the database is pinged at most once in
	// this time.
	DefaultMaxAge = time.Second
)

// ErrDraining is readiness from the moment shutdown began.
var ErrDraining = errors.New("health: shutting down")

// Options are a Readiness's settings; zero values take the defaults.
type Options struct {
	PingTimeout time.Duration
	MaxAge      time.Duration
	Now         func() time.Time
	// Logf says when the database stops answering and when it is back,
	// once each. Nil logs nothing.
	Logf func(format string, args ...any)
}

// Readiness answers whether the task should get traffic.
type Readiness struct {
	db       Database
	timeout  time.Duration
	maxAge   time.Duration
	now      func() time.Time
	logf     func(format string, args ...any)
	draining atomic.Bool

	mu      sync.Mutex
	checked time.Time
	last    error
	failing bool
}

// NewReadiness builds readiness over db. A nil db answers from draining
// alone.
func NewReadiness(db Database, options Options) *Readiness {
	r := &Readiness{
		db: db, timeout: options.PingTimeout, maxAge: options.MaxAge, now: options.Now, logf: options.Logf,
	}
	if r.timeout <= 0 {
		r.timeout = DefaultPingTimeout
	}
	if r.maxAge <= 0 {
		r.maxAge = DefaultMaxAge
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.logf == nil {
		r.logf = func(string, ...any) {}
	}
	return r
}

// Drain takes the task out: from now on Check answers ErrDraining. Shutdown
// calls it first, before it stops taking connections.
func (r *Readiness) Drain() {
	if r != nil {
		r.draining.Store(true)
	}
}

// Draining reports whether Drain was called.
func (r *Readiness) Draining() bool { return r != nil && r.draining.Load() }

// Check answers nil when the task should get traffic. A nil Readiness is
// always ready.
func (r *Readiness) Check(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if r.draining.Load() {
		return ErrDraining
	}
	if r.db == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.checked.IsZero() && now.Sub(r.checked) < r.maxAge {
		return r.last
	}
	// The answer is kept for the next callers, so it must not be this
	// caller's having hung up.
	pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.timeout)
	err := r.db.Ping(pingCtx)
	cancel()
	if err != nil {
		err = fmt.Errorf("health: database: %w", err)
	}
	switch {
	case err != nil && !r.failing:
		r.logf("readiness: not ready, the database does not answer: %v", err)
	case err == nil && r.failing:
		r.logf("readiness: ready again, the database answers")
	}
	r.checked, r.last, r.failing = now, err, err != nil
	return err
}
