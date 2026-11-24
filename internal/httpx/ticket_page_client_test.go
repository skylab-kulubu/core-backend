package httpx_test

import (
	"context"
	"net"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/handlers"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// siteProxies trusts the edge proxy and the club site by name, as
// production does (TRUSTED_PROXY_RANGES=dokploy-traefik,tasks.<site>);
// app.Test dials from 0.0.0.0, which the names resolve to when listed.
func siteProxies(t *testing.T, raw string, resolved ...string) clientip.Ranges {
	t.Helper()
	ranges, err := clientip.ParseRanges(raw)
	if err != nil {
		t.Fatal(err)
	}
	ranges.Refresh(t.Context(), func(_ context.Context, host string) ([]netip.Addr, error) {
		if slices.Contains(resolved, host) {
			return []netip.Addr{netip.MustParseAddr("0.0.0.0")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}, func(string, ...any) {})
	return ranges
}

func ticketPageApp(t *testing.T, proxies clientip.Ranges) *fiber.App {
	t.Helper()
	deps := memoryDeps()
	deps.TrustedProxies = proxies
	deps.TicketPage = ticketpage.New(ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), nil, nil, nil, ticketpage.Config{})
	return httpx.New(deps)
}

func visitTicket(t *testing.T, app *fiber.App, visitor, path string) int {
	t.Helper()
	request := httptest.NewRequest(fiber.MethodGet, path, nil)
	request.Header.Set(fiber.HeaderXForwardedFor, visitor)
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode
}

const ticketKey = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// The club site reads a ticket page's JSON from its own server, not from
// the visitor's browser (docs/ticket-page.md, "The site's server"). It
// calls core on the internal network and names the visitor in
// X-Forwarded-For; the site's tasks, named in TRUSTED_PROXY_RANGES, are
// believed (docs/client-ip-trust.md), so the ticket routes' budget follows
// each visitor and one of them cannot spend everybody's.
func TestPublicTicketLimitFollowsTheVisitorTheSiteNames(t *testing.T) {
	t.Parallel()
	app := ticketPageApp(t, siteProxies(t, "dokploy-traefik,tasks.site", "tasks.site"))
	const budget = 120
	for i := range budget {
		if status := visitTicket(t, app, "198.51.100.20", "/v1/public/tickets/"+ticketKey); status == fiber.StatusTooManyRequests {
			t.Fatalf("request %d of %d was limited early", i+1, budget)
		}
	}
	if status := visitTicket(t, app, "198.51.100.20", "/v1/public/tickets/"+ticketKey); status != fiber.StatusTooManyRequests {
		t.Fatalf("over-budget visitor status = %d, want 429", status)
	}
	// The PDF shares the page's budget per visitor.
	if status := visitTicket(t, app, "198.51.100.20", "/v1/public/tickets/"+ticketKey+"/pdf"); status != fiber.StatusTooManyRequests {
		t.Fatalf("over-budget visitor's PDF status = %d, want 429", status)
	}
	for _, path := range []string{"/v1/public/tickets/" + ticketKey, "/v1/public/tickets/" + ticketKey + "/pdf"} {
		if status := visitTicket(t, app, "203.0.113.9", path); status == fiber.StatusTooManyRequests {
			t.Fatalf("%s: a second visitor inherited the first one's budget", path)
		}
	}
}

// Another container on the network is not a proxy unless it is named: the
// header it writes is ignored and its own address counts, so everybody it
// names shares one budget.
func TestPublicTicketLimitIgnoresAnUnnamedContainer(t *testing.T) {
	t.Parallel()
	app := ticketPageApp(t, siteProxies(t, "dokploy-traefik,tasks.site"))
	for i := range 120 {
		visitor := netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}).String()
		if status := visitTicket(t, app, visitor, "/v1/public/tickets/"+ticketKey); status == fiber.StatusTooManyRequests {
			t.Fatalf("request %d was limited early", i+1)
		}
	}
	if status := visitTicket(t, app, "203.0.113.9", "/v1/public/tickets/"+ticketKey); status != fiber.StatusTooManyRequests {
		t.Fatalf("a header from an unnamed container was believed: status %d", status)
	}
}

// The page polls its check-ins every 4 seconds on the Event's day. Many
// holders at the door share one address, so the route's budget per client
// is large and the one that holds a page back is per key: the 31st request
// in a minute for one key is 429, from any address, while another key is
// still answered. Every answer is no-store.
func TestTicketCheckInsBudgetIsPerKey(t *testing.T) {
	t.Parallel()
	app := ticketPageApp(t, siteProxies(t, "dokploy-traefik,tasks.site", "tasks.site"))
	path := "/v1/public/tickets/" + ticketKey + "/check-ins"
	for i := range handlers.TicketCheckInsPerKeyMinute {
		visitor := netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}).String()
		request := httptest.NewRequest(fiber.MethodGet, path, nil)
		request.Header.Set(fiber.HeaderXForwardedFor, visitor)
		response, err := app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode == fiber.StatusTooManyRequests {
			t.Fatalf("request %d was limited early", i+1)
		}
		if got := response.Header.Get(fiber.HeaderCacheControl); got != "no-store" {
			t.Fatalf("Cache-Control %q", got)
		}
	}
	if status := visitTicket(t, app, "203.0.113.9", path); status != fiber.StatusTooManyRequests {
		t.Fatalf("over the key's budget: status %d, want 429", status)
	}
	other := "/v1/public/tickets/ZYXWVUTSRQPONMLKJIHGFEDCBA/check-ins"
	if status := visitTicket(t, app, "203.0.113.9", other); status == fiber.StatusTooManyRequests {
		t.Fatal("another key inherited the first one's budget")
	}
}

// The holder's cancel from the page (kontenjan-iptal-hatirlatma 03) has a
// small budget per client: the 11th request in a minute from one visitor
// is 429, another visitor is still answered. Every answer is no-store.
func TestTicketCancelBudgetIsPerClient(t *testing.T) {
	t.Parallel()
	app := ticketPageApp(t, siteProxies(t, "dokploy-traefik,tasks.site", "tasks.site"))
	path := "/v1/public/tickets/" + ticketKey + "/cancel"
	post := func(visitor string) (int, string) {
		t.Helper()
		request := httptest.NewRequest(fiber.MethodPost, path, nil)
		request.Header.Set(fiber.HeaderXForwardedFor, visitor)
		response, err := app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header.Get(fiber.HeaderCacheControl)
	}
	for i := range handlers.TicketCancelsPerClientMinute {
		status, cache := post("198.51.100.20")
		if status != fiber.StatusNotFound || cache != "no-store" {
			t.Fatalf("request %d: %d %q", i+1, status, cache)
		}
	}
	if status, _ := post("198.51.100.20"); status != fiber.StatusTooManyRequests {
		t.Fatalf("over the budget: status %d, want 429", status)
	}
	if status, _ := post("203.0.113.9"); status != fiber.StatusNotFound {
		t.Fatalf("another visitor: status %d", status)
	}
}
