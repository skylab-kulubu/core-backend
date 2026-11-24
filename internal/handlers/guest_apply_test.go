package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

// guestApplyApp is the Guest apply route as httpx wires it, without the
// optional bearer: every request here is token-less.
func guestApplyApp(t *testing.T, proxies clientip.Ranges, logs *lockedBuffer, metrics *GuestApplyMetrics) (*fiber.App, string) {
	t.Helper()
	events := event.NewMemoryStore()
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	guest := NewGuestApply(proxies, metrics, DefaultGuestApplyLimits(), log.New(logs, "", 0))
	h := NewTicketHandler(ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy())))
	app := fiber.New()
	app.Use(requestid.New())
	route := []any{guest.HideFromEdge}
	for _, limit := range guest.Limits() {
		route = append(route, limit)
	}
	app.Post("/v1/events/:eventId/applications/guest", guest.Observe, append(route, h.ApplyGuest)...)
	return app, "/v1/events/" + ev.ID.String() + "/applications/guest"
}

func TestGuestApplyLogsTheClassAndOutcomeButNoPersonalData(t *testing.T) {
	t.Parallel()
	proxies, err := clientip.ParseRanges("0.0.0.0/32")
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	metrics := NewGuestApplyMetrics()
	app, path := guestApplyApp(t, proxies, &logs, metrics)

	send := func(forwardedFor, body string) int {
		req := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(body))
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
		{"203.0.113.7", body, fiber.StatusNotFound},
		{"", `{"firstName":"Ada"}`, fiber.StatusBadRequest},
	} {
		if got := send(step.forwardedFor, step.body); got != step.status {
			t.Fatalf("status %d want %d", got, step.status)
		}
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	want := []struct{ caller, outcome string }{
		{"anonymous_internal", "created"},
		{"anonymous_public", "not_found"},
		{"anonymous_internal", "invalid"},
	}
	if len(lines) != len(want) {
		t.Fatalf("log lines %q", lines)
	}
	for i, line := range lines {
		for _, secret := range []string{"ada@example.com", "Ada", "Lovelace", "5551234", "203.0.113.7", path[len("/v1/events/"):][:36]} {
			if strings.Contains(withoutCorrelationID(t, line), secret) {
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
		`skylab_guest_apply_requests_total{caller="anonymous_public",outcome="not_found"} 1`,
		`skylab_guest_apply_requests_total{caller="service",outcome="created"} 0`,
		"skylab_guest_apply_anonymous_internal_total 2",
		"skylab_guest_apply_anonymous_public_total 1",
		"skylab_guest_apply_person_total 0",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("metrics lack %q:\n%s", line, text)
		}
	}
}

// A token-less request that came through the public ingress is answered a
// bare 404 before anything is read or written, whatever address it names:
// Traefik (a trusted proxy) adds a forwarding header to everything it passes
// on, a private visitor's address included, and a proxy whose name did not
// resolve (a private peer that is not trusted) still writes one. Only a
// private caller that sends none (the internal hop) reaches the handler. The
// app listens on the loopback, so the peer is a real private address.
func TestGuestApplyHidesTheRouteFromTheEdge(t *testing.T) {
	t.Parallel()
	for name, ranges := range map[string]string{
		"trusted proxy":    "127.0.0.1/32",
		"unresolved proxy": "dokploy-traefik", // never looked up here: as if it did not resolve
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			proxies, err := clientip.ParseRanges(ranges)
			if err != nil {
				t.Fatal(err)
			}
			var logs lockedBuffer
			app, path := guestApplyApp(t, proxies, &logs, NewGuestApplyMetrics())
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
			t.Cleanup(func() { _ = app.Shutdown() })

			send := func(header, value, email string) (int, http.Header, string) {
				body := `{"firstName":"Ada","lastName":"Lovelace","email":"` + email + `"}`
				req, err := http.NewRequestWithContext(t.Context(), fiber.MethodPost, "http://"+listener.Addr().String()+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				if header != "" {
					req.Header.Set(header, value)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				raw, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, resp.Header, string(raw)
			}
			for _, step := range []struct{ header, value string }{
				{"X-Forwarded-For", "203.0.113.7"},
				{"X-Forwarded-For", "10.0.1.7"},
				{"X-Real-Ip", "10.0.1.7"},
				{"X-Forwarded-Proto", "https"},
				{"Forwarded", "for=203.0.113.7"},
			} {
				status, header, body := send(step.header, step.value, "edge@example.com")
				if status != fiber.StatusNotFound || body != "Not Found" || header.Get(fiber.HeaderCacheControl) != "no-store" {
					t.Fatalf("%s: %d %q %v", step.header, status, body, header)
				}
			}
			if status, _, body := send("", "", "hop@example.com"); status != fiber.StatusCreated {
				t.Fatalf("internal hop: %d %s", status, body)
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			want := []string{"anonymous_public", "anonymous_public", "anonymous_public", "anonymous_public", "anonymous_public", "anonymous_internal"}
			if len(lines) != len(want) {
				t.Fatalf("log lines %q", lines)
			}
			for i, line := range lines {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("log line %q: %v", line, err)
				}
				if entry["caller"] != want[i] {
					t.Fatalf("request %d counted %v, want %s", i, entry["caller"], want[i])
				}
			}
		})
	}
}
