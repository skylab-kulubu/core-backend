package handlers

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestGuestApplyLogsTheClassAndOutcomeButNoPersonalData(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	proxies, err := clientip.ParseRanges("0.0.0.0/32")
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	metrics := NewGuestApplyMetrics()
	guest := NewGuestApply(proxies, metrics, GuestApplyLimits{PerClient: 1, PerClientWindow: time.Minute, PerGuest: 5, PerGuestWindow: time.Minute},
		log.New(&logs, "", 0))
	h := NewTicketHandler(ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy())))
	app := fiber.New(fiber.Config{
		TrustProxy:       true,
		TrustProxyConfig: fiber.TrustProxyConfig{Proxies: proxies.Proxies()},
		ProxyHeader:      fiber.HeaderXForwardedFor,
	})
	app.Use(requestid.New())
	route := []any{}
	for _, limit := range guest.Limits() {
		route = append(route, limit)
	}
	app.Post("/v1/events/:eventId/applications/guest", guest.Observe, append(route, h.ApplyGuest)...)

	send := func(forwardedFor, body string) int {
		req := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/applications/guest", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if forwardedFor != "" {
			req.Header.Set("X-Forwarded-For", forwardedFor)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	const body = `{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com","phoneNumber":"5551234"}`
	for _, step := range []struct {
		forwardedFor, body string
		status             int
	}{
		{"", body, fiber.StatusCreated},
		{"203.0.113.7", body, fiber.StatusCreated},
		{"203.0.113.7", body, fiber.StatusTooManyRequests},
		{"", `{"firstName":"Ada"}`, fiber.StatusBadRequest},
	} {
		if got := send(step.forwardedFor, step.body); got != step.status {
			t.Fatalf("status %d want %d", got, step.status)
		}
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	want := []struct{ caller, outcome string }{
		{"anonymous_internal", "created"},
		{"anonymous_public", "existing"},
		{"anonymous_public", "rate_limited"},
		{"anonymous_internal", "invalid"},
	}
	if len(lines) != len(want) {
		t.Fatalf("log lines %q", lines)
	}
	for i, line := range lines {
		for _, secret := range []string{"ada@example.com", "Ada", "Lovelace", "5551234", "203.0.113.7", ev.ID.String()} {
			if strings.Contains(line, secret) {
				t.Fatalf("log line %q carries %q", line, secret)
			}
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if entry["event"] != "guest_apply" || entry["caller"] != want[i].caller || entry["outcome"] != want[i].outcome ||
			entry["correlation_id"] == "" || len(entry) != 5 {
			t.Fatalf("log line %d %v want %+v", i, entry, want[i])
		}
	}

	text := metrics.Prometheus()
	if strings.Count(text, "skylab_guest_apply_requests_total{") != int(guestCallerCount)*int(guestOutcomeCount) {
		t.Fatalf("every class and outcome is rendered, zero included:\n%s", text)
	}
	for _, line := range []string{
		"# TYPE skylab_guest_apply_requests_total counter",
		`skylab_guest_apply_requests_total{caller="anonymous_internal",outcome="created"} 1`,
		`skylab_guest_apply_requests_total{caller="anonymous_public",outcome="rate_limited"} 1`,
		`skylab_guest_apply_requests_total{caller="service",outcome="created"} 0`,
		"skylab_guest_apply_anonymous_internal_total 2",
		"skylab_guest_apply_anonymous_public_total 2",
		"skylab_guest_apply_person_total 0",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("metrics lack %q:\n%s", line, text)
		}
	}
}

func TestParseGuestApplyLimitMode(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]GuestApplyLimitMode{
		"": GuestApplyLimitEnforce, " enforce ": GuestApplyLimitEnforce, "observe": GuestApplyLimitObserve,
	} {
		got, err := ParseGuestApplyLimitMode(raw)
		if err != nil || got != want {
			t.Fatalf("%q: %v %v, want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"off", "Enforce", "log"} {
		if _, err := ParseGuestApplyLimitMode(raw); err == nil {
			t.Fatalf("%q accepted", raw)
		}
	}
}
