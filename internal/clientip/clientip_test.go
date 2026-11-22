package clientip_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
)

// The overlay network the edge proxy lives on. Its own address inside that
// network is reassigned whenever the container is recreated, so every case
// below identifies it by range.
const overlay = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8,::1/128,fc00::/7"

func TestResolve(t *testing.T) {
	t.Parallel()

	ranges, err := clientip.ParseRanges(overlay)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		trusted   bool
		peer      string
		forwarded []string
		want      string
	}{
		{
			name:      "trusted proxy forwards a single client",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"203.0.113.9"},
			want:      "203.0.113.9",
		},
		{
			name:      "trusted proxy keeps working after it moves inside the network",
			trusted:   true,
			peer:      "10.0.1.157",
			forwarded: []string{"203.0.113.9"},
			want:      "203.0.113.9",
		},
		{
			name:      "chain returns the address the outermost proxy observed",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"203.0.113.9", "10.0.1.42"},
			want:      "203.0.113.9",
		},
		{
			name:      "a forged leftmost entry never becomes the client",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"1.2.3.4", "198.51.100.20"},
			want:      "198.51.100.20",
		},
		{
			name:      "an untrusted peer cannot forward anything",
			trusted:   false,
			peer:      "198.51.100.20",
			forwarded: []string{"1.2.3.4"},
			want:      "198.51.100.20",
		},
		{
			name:      "an untrusted peer that spoofs a trusted-looking chain is still itself",
			trusted:   false,
			peer:      "198.51.100.20",
			forwarded: []string{"1.2.3.4", "10.0.1.109"},
			want:      "198.51.100.20",
		},
		{
			name:      "ipv6 client",
			trusted:   true,
			peer:      "fd00::1",
			forwarded: []string{"2001:db8::1"},
			want:      "2001:db8::1",
		},
		{
			name:      "bracketed ipv6 with a port",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"[2001:db8::1]:51234"},
			want:      "2001:db8::1",
		},
		{
			name:      "ipv4 with a port",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"203.0.113.9:443"},
			want:      "203.0.113.9",
		},
		{
			name:      "ipv4-mapped ipv6 collapses to one spelling",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"::ffff:203.0.113.9"},
			want:      "203.0.113.9",
		},
		{
			name:      "an ipv6 trusted range is honored",
			trusted:   true,
			peer:      "fd00::1",
			forwarded: []string{"2001:db8::1", "fd00::2"},
			want:      "2001:db8::1",
		},
		{
			name:      "a broken chain falls back to the socket address",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"203.0.113.9", "not-an-ip"},
			want:      "10.0.1.109",
		},
		{
			name:      "a chain of only trusted proxies falls back to the socket address",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"10.0.1.42", "127.0.0.1"},
			want:      "10.0.1.109",
		},
		{
			name:      "an empty chain falls back to the socket address",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: nil,
			want:      "10.0.1.109",
		},
		{
			name:      "blank entries are skipped",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"203.0.113.9", "  "},
			want:      "203.0.113.9",
		},
		{
			name:      "garbage everywhere stores nothing",
			trusted:   true,
			peer:      "not-an-ip",
			forwarded: []string{"<script>"},
			want:      "",
		},
		{
			name:      "an address with a zone is not an identity",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"fe80::1%eth0"},
			want:      "10.0.1.109",
		},
		{
			name:      "a hostname is never stored",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"client.example.test"},
			want:      "10.0.1.109",
		},
		{
			name:      "unknown token for a lost hop",
			trusted:   true,
			peer:      "10.0.1.109",
			forwarded: []string{"203.0.113.9", "unknown"},
			want:      "10.0.1.109",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clientip.Resolve(tc.trusted, tc.peer, tc.forwarded, ranges); got != tc.want {
				t.Fatalf("Resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveWithoutTrustedRangesNeverReadsTheHeader(t *testing.T) {
	t.Parallel()

	var none clientip.Ranges
	if !none.Empty() {
		t.Fatal("the zero value trusted a proxy")
	}
	if got := clientip.Resolve(false, "10.0.1.109", []string{"1.2.3.4"}, none); got != "10.0.1.109" {
		t.Fatalf("Resolve() = %q, want the socket address", got)
	}
}

func TestParseRanges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{
			name: "the documented default",
			raw:  overlay,
			want: strings.Split(overlay, ","),
		},
		{
			name: "surrounding whitespace and empty fields",
			raw:  " 10.0.0.0/8 ,, 127.0.0.0/8 ",
			want: []string{"10.0.0.0/8", "127.0.0.0/8"},
		},
		{
			name: "a bare address is a single host",
			raw:  "10.0.1.109",
			want: []string{"10.0.1.109"},
		},
		{
			name:    "a typo is rejected instead of ignored",
			raw:     "10.0.0.0/8,10.0.0/8",
			wantErr: true,
		},
		{
			name: "a host name is a proxy looked up by name",
			raw:  "dokploy-traefik, tasks.Site_1 ,127.0.0.0/8",
			want: []string{"dokploy-traefik", "tasks.site_1", "127.0.0.0/8"},
		},
		{
			name:    "a mistyped address is not taken for a name",
			raw:     "10.0.1.300",
			wantErr: true,
		},
		{
			name:    "a name with a space or a scheme is rejected",
			raw:     "http://traefik",
			wantErr: true,
		},
		{
			name:    "an empty list trusts nobody by accident",
			raw:     " , ",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ranges, err := clientip.ParseRanges(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRanges(%q) accepted an invalid list", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := ranges.Entries(); !slices.Equal(got, tc.want) {
				t.Fatalf("Entries() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParsedBareAddressMatchesOnlyItself(t *testing.T) {
	t.Parallel()

	ranges, err := clientip.ParseRanges("10.0.1.109")
	if err != nil {
		t.Fatal(err)
	}
	if got := clientip.Resolve(true, "10.0.1.109", []string{"203.0.113.9", "10.0.1.109"}, ranges); got != "203.0.113.9" {
		t.Fatalf("Resolve() = %q, want the client behind the single trusted proxy", got)
	}
	if got := clientip.Resolve(true, "10.0.1.109", []string{"203.0.113.9", "10.0.1.42"}, ranges); got != "10.0.1.42" {
		t.Fatalf("Resolve() = %q, want the untrusted neighbour that appended last", got)
	}
}

func TestRangesFromEnv(t *testing.T) {
	t.Parallel()

	unset, err := clientip.RangesFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if got, want := unset.String(), clientip.DefaultRanges; got != want {
		t.Fatalf("unset %s = %q, want %q", clientip.EnvKey, got, want)
	}

	configured, err := clientip.RangesFromEnv(func(key string) string {
		if key != clientip.EnvKey {
			t.Fatalf("read %q instead of %q", key, clientip.EnvKey)
		}
		return "10.0.1.0/24"
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.String(); got != "10.0.1.0/24" {
		t.Fatalf("%s = %q", clientip.EnvKey, got)
	}

	if _, err := clientip.RangesFromEnv(func(string) string { return "10.0.1.0/33" }); err == nil {
		t.Fatalf("an invalid %s started the process", clientip.EnvKey)
	}
}

// A request is resolved end to end: whether the peer is a proxy and which
// address in the chain it observed.
func TestFromCtxFollowsTheAppTrustConfiguration(t *testing.T) {
	t.Parallel()

	// app.Test dials from 0.0.0.0, so that address stands in for the proxy.
	trusting := appWithProxies(t, "0.0.0.0/32,"+overlay)
	req := httptest.NewRequest(fiber.MethodGet, "/", nil)
	req.Header.Set(fiber.HeaderXForwardedFor, "1.2.3.4, 198.51.100.20")
	if got := probe(t, trusting, req); got != "198.51.100.20" {
		t.Fatalf("trusted peer resolved %q", got)
	}

	repeated := httptest.NewRequest(fiber.MethodGet, "/", nil)
	repeated.Header.Add(fiber.HeaderXForwardedFor, "1.2.3.4")
	repeated.Header.Add(fiber.HeaderXForwardedFor, "198.51.100.20, 10.0.1.42")
	if got := probe(t, trusting, repeated); got != "198.51.100.20" {
		t.Fatalf("repeated header lines resolved %q", got)
	}

	untrusting := appWithProxies(t, overlay)
	spoofed := httptest.NewRequest(fiber.MethodGet, "/", nil)
	spoofed.Header.Set(fiber.HeaderXForwardedFor, "1.2.3.4")
	if got := probe(t, untrusting, spoofed); got != "0.0.0.0" {
		t.Fatalf("untrusted peer resolved %q, want its socket address", got)
	}
}

func appWithProxies(t *testing.T, raw string) *fiber.App {
	t.Helper()
	ranges, err := clientip.ParseRanges(raw)
	if err != nil {
		t.Fatal(err)
	}
	return appTrusting(ranges)
}

// appTrusting answers the resolved address; it asks twice, as a limiter and
// its handler do.
func appTrusting(ranges clientip.Ranges) *fiber.App {
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		clientip.FromCtx(c, ranges)
		return c.SendString(clientip.FromCtx(c, ranges))
	})
	return app
}

func probe(t *testing.T, app *fiber.App, req *http.Request) string {
	t.Helper()
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test body close
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// fakeDNS answers the names it holds; others are not found. fail makes a
// name's lookup fail without saying it is gone (a DNS hiccup).
type fakeDNS struct {
	mu    sync.Mutex
	names map[string][]string
	fail  map[string]bool
}

func (f *fakeDNS) set(name string, addrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names == nil {
		f.names = map[string][]string{}
	}
	if addrs == nil {
		delete(f.names, name)
		return
	}
	f.names[name] = addrs
}

func (f *fakeDNS) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[host] {
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
	}
	raw, ok := f.names[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]netip.Addr, 0, len(raw))
	for _, a := range raw {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.lines
	l.lines = nil
	return out
}

// The edge proxy and the site are containers on the same overlay subnet as
// every other container: they are trusted by name, for the addresses their
// names resolve to now, and nobody else in the subnet is.
func TestNamedProxiesAreTrustedForWhatTheyResolveTo(t *testing.T) {
	t.Parallel()
	ranges, err := clientip.ParseRanges("dokploy-traefik,tasks.site")
	if err != nil {
		t.Fatal(err)
	}
	traefik, site, neighbour := netip.MustParseAddr("10.0.1.5"), netip.MustParseAddr("10.0.1.9"), netip.MustParseAddr("10.0.1.7")
	if ranges.Contains(traefik) {
		t.Fatal("a name was trusted before it was looked up")
	}
	dns := &fakeDNS{}
	dns.set("dokploy-traefik", "10.0.1.5")
	dns.set("tasks.site", "10.0.1.9", "::ffff:10.0.1.10")
	var logs logLines
	ranges.Refresh(t.Context(), dns.lookup, logs.printf)
	if !ranges.Contains(traefik) || !ranges.Contains(site) || !ranges.Contains(netip.MustParseAddr("10.0.1.10")) {
		t.Fatal("a resolved proxy is not trusted")
	}
	if ranges.Contains(neighbour) {
		t.Fatal("a neighbour on the same subnet is trusted")
	}
	if got := logs.take(); !slices.Equal(got, []string{"trusted proxy dokploy-traefik: 1 address(es)", "trusted proxy tasks.site: 2 address(es)"}) {
		t.Fatalf("logs %q", got)
	}
	ranges.Refresh(t.Context(), dns.lookup, logs.printf)
	if got := logs.take(); len(got) != 0 {
		t.Fatalf("an unchanged answer was logged: %q", got)
	}

	// A DNS hiccup keeps the proxy; the proxy gone from DNS loses its
	// address, which the runtime may hand to another container.
	dns.fail = map[string]bool{"dokploy-traefik": true}
	ranges.Refresh(t.Context(), dns.lookup, logs.printf)
	if !ranges.Contains(traefik) {
		t.Fatal("a failed lookup dropped the proxy")
	}
	dns.fail = nil
	dns.set("dokploy-traefik")
	ranges.Refresh(t.Context(), dns.lookup, logs.printf)
	if ranges.Contains(traefik) || !ranges.Contains(site) {
		t.Fatal("a proxy gone from DNS is still trusted, or took the other one with it")
	}
	// Recreated elsewhere: the new address only.
	dns.set("dokploy-traefik", "10.0.1.6")
	ranges.Refresh(t.Context(), dns.lookup, logs.printf)
	if ranges.Contains(traefik) || !ranges.Contains(netip.MustParseAddr("10.0.1.6")) {
		t.Fatal("the recreated proxy's address did not replace the old one")
	}
	got := strings.Join(logs.take(), "\n")
	for _, want := range []string{"lookup failed", "keeping 1 address(es)", "not resolved", "1 address(es)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logs %q lack %q", got, want)
		}
	}
	if strings.Contains(got, "10.0.1.") {
		t.Fatalf("logs name an address: %q", got)
	}
}

// Guest apply's internal hop is any private address, whether or not it is
// a proxy; a proxy is internal too.
func TestInternalIsThePrivateNetwork(t *testing.T) {
	t.Parallel()
	ranges, err := clientip.ParseRanges("0.0.0.0/32,dokploy-traefik")
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"10.0.1.7": true, "172.18.0.3": true, "127.0.0.1": true, "fd00::1": true, "0.0.0.0": true,
		"203.0.113.9": false, "2001:db8::1": false,
	} {
		if got := ranges.Internal(netip.MustParseAddr(addr)); got != want {
			t.Fatalf("Internal(%s) = %v, want %v", addr, got, want)
		}
	}
}

// app.Test dials from 0.0.0.0: a name that resolves there is the proxy.
func TestFromCtxBelievesANamedProxyAndCountsIgnoredHeaders(t *testing.T) {
	t.Parallel()
	dns := &fakeDNS{}
	dns.set("site", "0.0.0.0")
	named, err := clientip.ParseRanges("site")
	if err != nil {
		t.Fatal(err)
	}
	named.Refresh(t.Context(), dns.lookup, func(string, ...any) {})
	req := httptest.NewRequest(fiber.MethodGet, "/", nil)
	req.Header.Set(fiber.HeaderXForwardedFor, "198.51.100.20")
	if got := probe(t, appTrusting(named), req); got != "198.51.100.20" {
		t.Fatalf("named proxy resolved %q", got)
	}

	other, err := clientip.ParseRanges("dokploy-traefik")
	if err != nil {
		t.Fatal(err)
	}
	other.Refresh(t.Context(), dns.lookup, func(string, ...any) {})
	app := appTrusting(other)
	for range 2 {
		spoofed := httptest.NewRequest(fiber.MethodGet, "/", nil)
		spoofed.Header.Set(fiber.HeaderXForwardedFor, "198.51.100.20")
		if got := probe(t, app, spoofed); got != "0.0.0.0" {
			t.Fatalf("a peer the name does not resolve to resolved %q", got)
		}
	}
	plain := httptest.NewRequest(fiber.MethodGet, "/", nil)
	if got := probe(t, app, plain); got != "0.0.0.0" {
		t.Fatalf("no header resolved %q", got)
	}
	text := other.Prometheus()
	for _, want := range []string{
		"skylab_client_ip_forwarded_ignored_total 2\n",
		`skylab_trusted_proxy_addresses{name="dokploy-traefik"} 0` + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics lack %q:\n%s", want, text)
		}
	}
	if text := named.Prometheus(); !strings.Contains(text, `skylab_trusted_proxy_addresses{name="site"} 1`) || !strings.Contains(text, "skylab_client_ip_forwarded_ignored_total 0") {
		t.Fatalf("named metrics:\n%s", text)
	}
}

// A recreated proxy first shows as a header from a peer that is not trusted:
// Watch looks the names up again then, without waiting for its tick.
func TestWatchLooksUpAgainWhenAHeaderIsIgnored(t *testing.T) {
	restore := clientip.SetRefreshTimings(time.Hour, 0)
	defer restore()
	dns := &fakeDNS{}
	ranges, err := clientip.ParseRanges("dokploy-traefik")
	if err != nil {
		t.Fatal(err)
	}
	ranges.Refresh(t.Context(), dns.lookup, func(string, ...any) {})
	ctx, cancel := context.WithCancel(t.Context())
	done := ranges.Watch(ctx, dns.lookup, func(string, ...any) {})
	defer func() {
		cancel()
		<-done
	}()
	dns.set("dokploy-traefik", "0.0.0.0")
	app := appTrusting(ranges)
	req := httptest.NewRequest(fiber.MethodGet, "/", nil)
	req.Header.Set(fiber.HeaderXForwardedFor, "198.51.100.20")
	if got := probe(t, app, req); got != "0.0.0.0" {
		t.Fatalf("before the lookup resolved %q", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !ranges.Contains(netip.MustParseAddr("0.0.0.0")) {
		if time.Now().After(deadline) {
			t.Fatal("an ignored header did not bring a lookup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	again := httptest.NewRequest(fiber.MethodGet, "/", nil)
	again.Header.Set(fiber.HeaderXForwardedFor, "198.51.100.20")
	if got := probe(t, app, again); got != "198.51.100.20" {
		t.Fatalf("after the lookup resolved %q", got)
	}
}
