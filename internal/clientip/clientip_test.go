package clientip_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
			name:    "a hostname is rejected",
			raw:     "traefik",
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
			got := ranges.Proxies()
			if len(got) != len(tc.want) {
				t.Fatalf("Proxies() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Proxies() = %v, want %v", got, tc.want)
				}
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

	if _, err := clientip.RangesFromEnv(func(string) string { return "not-a-range" }); err == nil {
		t.Fatalf("an invalid %s started the process", clientip.EnvKey)
	}
}

// Fiber decides whether the peer is a proxy; this package decides which address
// in the chain that proxy observed. The two read the same configuration, and a
// request is resolved end to end here to prove they stay in agreement.
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
	app := fiber.New(fiber.Config{
		TrustProxy:         true,
		TrustProxyConfig:   fiber.TrustProxyConfig{Proxies: ranges.Proxies()},
		ProxyHeader:        fiber.HeaderXForwardedFor,
		EnableIPValidation: true,
	})
	app.Get("/", func(c fiber.Ctx) error {
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
