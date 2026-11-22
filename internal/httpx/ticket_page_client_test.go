package httpx_test

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The club site reads a ticket page's JSON from its own server, not from
// the visitor's browser (docs/ticket-page.md, "The site's server"). It
// calls core on the internal network and names the visitor in
// X-Forwarded-For; a peer inside TRUSTED_PROXY_RANGES is believed
// (docs/client-ip-trust.md), so the ticket routes' budget follows each
// visitor and one of them cannot spend everybody's. A visitor's own
// X-Forwarded-For reaches core only through such a peer.
func TestPublicTicketLimitFollowsTheVisitorTheSiteNames(t *testing.T) {
	deps := memoryDeps()
	deps.TicketPage = ticketpage.New(ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), nil, nil, nil, ticketpage.Config{})
	app := httpx.New(deps)
	const key = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	visit := func(visitor, path string) int {
		t.Helper()
		request := httptest.NewRequest(fiber.MethodGet, path, nil)
		request.Header.Set(fiber.HeaderXForwardedFor, visitor)
		response, err := app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode
	}
	const budget = 120
	for i := range budget {
		if status := visit("198.51.100.20", "/v1/public/tickets/"+key); status == fiber.StatusTooManyRequests {
			t.Fatalf("request %d of %d was limited early", i+1, budget)
		}
	}
	if status := visit("198.51.100.20", "/v1/public/tickets/"+key); status != fiber.StatusTooManyRequests {
		t.Fatalf("over-budget visitor status = %d, want 429", status)
	}
	// The PDF shares the page's budget per visitor.
	if status := visit("198.51.100.20", "/v1/public/tickets/"+key+"/pdf"); status != fiber.StatusTooManyRequests {
		t.Fatalf("over-budget visitor's PDF status = %d, want 429", status)
	}
	for _, path := range []string{"/v1/public/tickets/" + key, "/v1/public/tickets/" + key + "/pdf"} {
		if status := visit("203.0.113.9", path); status == fiber.StatusTooManyRequests {
			t.Fatalf("%s: a second visitor inherited the first one's budget", path)
		}
	}
}
