package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/health"
)

// The shutdown budget. Docker sends SIGTERM, waits the service's
// stop_grace_period (10 s unless set) and then sends SIGKILL, so
// shutdownTimeout must stay under it with room to spare: run core with
// stop_grace_period 30 s (docs/health-and-shutdown.md).
const (
	// httpDrainTimeout is how long requests in flight get to finish. A
	// request still open after it is cut off.
	httpDrainTimeout = 20 * time.Second
	// shutdownTimeout is the whole shutdown, from the signal until serve
	// returns and the process exits: the drain, then the background
	// workers, the confirmation mails going and the pools.
	shutdownTimeout = 25 * time.Second
)

// stopSignals is the context serve watches: it ends on SIGTERM (Docker's
// stop) or SIGINT (Ctrl-C). Calling release restores the default, so a
// second signal kills at once.
func stopSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
}

// stopping is something shutdown waits on: a background worker's loop, or
// the confirmation mails going. done is asked once the requests have
// drained and the workers were told to stop, and its channel closes when
// it has stopped.
type stopping struct {
	name string
	done func() <-chan struct{}
}

// stopped is a stopping whose channel is known from the start (a worker's
// loop).
func stopped(name string, done <-chan struct{}) stopping {
	return stopping{name: name, done: func() <-chan struct{} { return done }}
}

// closing is something shutdown closes once the workers have stopped: the
// access gate's Redis client, a database pool.
type closing struct {
	name  string
	close func()
}

// shutdownPlan is what serve does once its context ends.
type shutdownPlan struct {
	// Readiness answers 503 from the first moment, so nothing takes this
	// task for one that can serve. Nil skips it.
	Readiness *health.Readiness
	// HTTPDrain bounds the wait for requests in flight; Total bounds the
	// whole shutdown.
	HTTPDrain time.Duration
	Total     time.Duration
	// StopWorkers cancels the background workers' context. They keep
	// working while requests drain and are stopped after.
	StopWorkers context.CancelFunc
	// Wait are waited on after StopWorkers, until Total runs out.
	Wait []stopping
	// Close are closed in order after Wait, until Total runs out.
	Close []closing
	Logf  func(format string, args ...any)
}

// serve answers HTTP on ln until ctx ends, then shuts down in order:
// readiness says 503; the port stops taking connections; idle keep-alive
// connections are closed and each request in flight is answered with
// Connection: close, for up to HTTPDrain; the background workers are
// stopped and waited on, with whatever else is in Wait (a worker settles
// or releases its claim as it stops); then what is in Close is closed, in
// order (the pools last). Whatever has not stopped or closed by Total is
// named in the log and left behind, and serve returns: the caller exits
// at once (os.Exit), before Docker's SIGKILL. Swarm
// takes a task out of its load balancer (and waits about two seconds)
// before it sends SIGTERM, so closing the port turns away no new
// connection. It returns an error only when serving fails.
func serve(ctx context.Context, app *fiber.App, ln net.Listener, plan shutdownPlan) error {
	logf := plan.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// fasthttp otherwise answers the last request of a keep-alive
	// connection without saying it closes, and the proxy may send the
	// next request into a closing socket.
	app.Server().CloseOnShutdown = true
	served := make(chan error, 1)
	if ctx.Err() == nil {
		go func() { served <- app.Listener(ln) }()
		select {
		case err := <-served:
			return err
		case <-ctx.Done():
		}
	} else {
		served <- nil
	}

	started := time.Now()
	deadline := started.Add(plan.Total)
	plan.Readiness.Drain()
	logf("shutdown: stop signal; not ready, taking no new connections, waiting up to %s for requests in flight", plan.HTTPDrain)
	if err := app.ShutdownWithTimeout(plan.HTTPDrain); err != nil && !errors.Is(err, fiber.ErrNotRunning) {
		logf("shutdown: requests still open after %s were cut off: %v", plan.HTTPDrain, err)
	}
	// Serving may not have begun when the signal came; a closed listener
	// makes it return at once all the same.
	_ = ln.Close()
	<-served

	if plan.StopWorkers != nil {
		plan.StopWorkers()
	}
	waitStopped(plan.Wait, deadline, logf)
	closeBy(plan.Close, deadline, logf)
	logf("shutdown: done in %s", time.Since(started).Round(time.Millisecond))
	return nil
}

// waitStopped waits for each of list until deadline, naming in the log the
// ones that had not stopped by then.
func waitStopped(list []stopping, deadline time.Time, logf func(string, ...any)) {
	wait, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, s := range list {
		select {
		case <-s.done():
		case <-wait.Done():
			logf("shutdown: %s did not stop by the deadline; what it held is taken up again when its lease runs out", s.name)
		}
	}
}

// closeBy closes each of list in order until deadline. A close still
// running then (pgxpool's Close waits for every connection in use) is
// named in the log and left behind with the ones after it.
func closeBy(list []closing, deadline time.Time, logf func(string, ...any)) {
	wait, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, c := range list {
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			c.close()
		}()
		select {
		case <-closed:
		case <-wait.Done():
			logf("shutdown: closing %s did not finish by the deadline; exiting without it", c.name)
			return
		}
	}
}
