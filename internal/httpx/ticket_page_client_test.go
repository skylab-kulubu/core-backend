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
