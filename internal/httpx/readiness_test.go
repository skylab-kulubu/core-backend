package httpx_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/health"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
)

type switchableDatabase struct {
	mu  sync.Mutex
	err error
}

func (d *switchableDatabase) Ping(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *switchableDatabase) set(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

// /v1/ready follows the database and draining, and keeps its 204 for the
// release wizards; /v1/health (liveness) answers 204 throughout and asks
// nothing.
func TestReadyFollowsTheDatabaseAndDrainingWhileHealthStaysUp(t *testing.T) {
	t.Parallel()
	db := &switchableDatabase{}
	// A max age of a nanosecond: every request pings.
	readiness := health.NewReadiness(db, health.Options{MaxAge: 1})
	deps := memoryDeps()
	deps.Readiness = readiness
	app := httpx.New(deps)

	status := func(path string) (int, string, string) {
		t.Helper()
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header.Get(fiber.HeaderCacheControl), response.Header.Get(fiber.HeaderRetryAfter)
	}

	for _, step := range []struct {
		name   string
		before func()
		ready  int
	}{
		{name: "database up", before: func() {}, ready: fiber.StatusNoContent},
		{name: "database down", before: func() { db.set(errors.New("connection refused")) }, ready: fiber.StatusServiceUnavailable},
		{name: "database back", before: func() { db.set(nil) }, ready: fiber.StatusNoContent},
		{name: "draining", before: readiness.Drain, ready: fiber.StatusServiceUnavailable},
	} {
		step.before()
		code, cacheControl, retryAfter := status("/v1/ready")
		if code != step.ready {
			t.Fatalf("%s: GET /v1/ready = %d, want %d", step.name, code, step.ready)
		}
		if code == fiber.StatusServiceUnavailable && (cacheControl != "no-store" || retryAfter == "") {
			t.Fatalf("%s: 503 with Cache-Control %q Retry-After %q", step.name, cacheControl, retryAfter)
		}
		if code, _, _ := status("/v1/health"); code != fiber.StatusNoContent {
			t.Fatalf("%s: GET /v1/health = %d, want 204", step.name, code)
		}
	}
}

// A database that is down answers 503 before the gate is asked, and the
// gate's readiness metric counts only the gate's own failures.
func TestReadyAsksTheGateOnlyWhenTheDatabaseAnswers(t *testing.T) {
	t.Parallel()
	db := &switchableDatabase{err: errors.New("down")}
	gate := &httpAccessGate{}
	deps := memoryDeps()
	deps.AccountAccessGate = gate
	deps.Readiness = health.NewReadiness(db, health.Options{MaxAge: 1})
	app := httpx.New(deps)
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/ready", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusServiceUnavailable || gate.readyCalls != 0 {
		t.Fatalf("status=%d gate calls=%d, want 503 and 0", response.StatusCode, gate.readyCalls)
	}
	db.set(nil)
	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/ready", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusNoContent || gate.readyCalls != 1 {
		t.Fatalf("status=%d gate calls=%d, want 204 and 1", response.StatusCode, gate.readyCalls)
	}
}

// The container's health check asks /v1/ready?gate=skip: the task and its
// database only. A gate Redis that is down fails /v1/ready, as before, but
// not the health check, so Swarm does not restart core for it.
func TestReadyGateSkipAsksTheTaskAndDatabaseOnly(t *testing.T) {
	t.Parallel()
	db := &switchableDatabase{}
	gate := &httpAccessGate{readyErr: errors.New("redis unavailable")}
	readiness := health.NewReadiness(db, health.Options{MaxAge: 1})
	deps := memoryDeps()
	deps.AccountAccessGate = gate
	deps.Readiness = readiness
	app := httpx.New(deps)
	status := func(path string) int {
		t.Helper()
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode
	}
	if got := status("/v1/ready"); got != fiber.StatusServiceUnavailable {
		t.Fatalf("/v1/ready with the gate down = %d, want 503", got)
	}
	if got := status("/v1/ready?gate=skip"); got != fiber.StatusNoContent || gate.readyCalls != 1 {
		t.Fatalf("/v1/ready?gate=skip with the gate down = %d (gate asked %d times), want 204 and the gate asked once (by /v1/ready)", got, gate.readyCalls)
	}
	db.set(errors.New("down"))
	if got := status("/v1/ready?gate=skip"); got != fiber.StatusServiceUnavailable {
		t.Fatalf("/v1/ready?gate=skip with the database down = %d, want 503", got)
	}
	db.set(nil)
	readiness.Drain()
	if got := status("/v1/ready?gate=skip"); got != fiber.StatusServiceUnavailable {
		t.Fatalf("/v1/ready?gate=skip while draining = %d, want 503", got)
	}
	// Any other value is the full check.
	deps.Readiness = health.NewReadiness(db, health.Options{MaxAge: 1})
	app = httpx.New(deps)
	if got := status("/v1/ready?gate=no"); got != fiber.StatusServiceUnavailable {
		t.Fatalf("/v1/ready?gate=no = %d, want the full check's 503", got)
	}
}
