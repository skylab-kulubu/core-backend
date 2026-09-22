// Package clientip decides when a forwarded-for header may be believed and
// resolves the address of the party that actually reached the edge proxy.
//
// The service runs behind a Traefik container that terminates TLS on a private
// overlay network. Traefik discards whatever `X-Forwarded-For` the caller sent
// and writes its own, so the header is worth reading exactly when the peer that
// opened the connection is one of the proxies an operator listed in
// TRUSTED_PROXY_RANGES. Traefik's address on that network is assigned by the
// container runtime and changes whenever it is recreated, which is why the
// configuration is a set of ranges and never a single address.
package clientip

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// EnvKey is the environment variable that carries the trusted proxy ranges.
const EnvKey = "TRUSTED_PROXY_RANGES"

// DefaultRanges covers the private and loopback space a container network hands
// out. It deliberately does not name a single proxy address: the edge proxy
// moves within the overlay network across restarts.
const DefaultRanges = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8,::1/128,fc00::/7"

// Ranges is a validated set of proxy addresses and networks.
//
// The zero value trusts nobody, which makes every request fall back to its
// socket address rather than to a header a client can write.
type Ranges struct {
	configured []string
	prefixes   []netip.Prefix
}

var defaultRanges = mustParseRanges(DefaultRanges)

// Default returns the parsed DefaultRanges.
func Default() Ranges { return defaultRanges }

// ParseRanges reads a comma-separated list of CIDR ranges. A bare address is
// accepted as a single-host range. An entry that is neither is an error rather
// than a silently ignored line, because a typo here would quietly make the
// service read a spoofable header or stop reading a real one.
func ParseRanges(raw string) (Ranges, error) {
	var ranges Ranges
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			addr, addrErr := netip.ParseAddr(field)
			if addrErr != nil || addr.Zone() != "" {
				return Ranges{}, fmt.Errorf("%s: %q is neither a CIDR range nor an IP address", EnvKey, field)
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		ranges.configured = append(ranges.configured, field)
		ranges.prefixes = append(ranges.prefixes, prefix.Masked())
	}
	if len(ranges.prefixes) == 0 {
		return Ranges{}, fmt.Errorf("%s must list at least one CIDR range", EnvKey)
	}
	return ranges, nil
}

// RangesFromEnv reads EnvKey, falling back to DefaultRanges when it is unset or
// blank.
func RangesFromEnv(getenv func(string) string) (Ranges, error) {
	raw := strings.TrimSpace(getenv(EnvKey))
	if raw == "" {
		return Default(), nil
	}
	return ParseRanges(raw)
}

// Proxies returns the configured entries in the spelling Fiber's
// TrustProxyConfig expects, so the framework and this package always agree on
// which peers are proxies.
func (r Ranges) Proxies() []string {
	proxies := make([]string, len(r.configured))
	copy(proxies, r.configured)
	return proxies
}

// Empty reports whether no proxy is trusted.
func (r Ranges) Empty() bool { return len(r.prefixes) == 0 }

// Contains reports whether addr belongs to one of the trusted networks.
func (r Ranges) Contains(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	for _, prefix := range r.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// String renders the configured entries for startup logs.
func (r Ranges) String() string { return strings.Join(r.configured, ",") }

// Resolve returns the client address as the outermost trusted proxy observed
// it.
//
// peer is the socket address this process accepted the connection from, and
// forwarded is the `X-Forwarded-For` chain in header order: the end a client
// controls first, the end a proxy appends last.
//
// When peer is not a trusted proxy the header is ignored outright, because
// anybody can send one. When peer is trusted the chain is walked from the
// right, and the first entry that is not itself a trusted proxy is the address
// that proxy accepted its connection from. Entries to the left of it were
// supplied by whoever sits behind that hop, so a forged leftmost entry can
// never become the recorded client. A chain that is empty, entirely made of
// trusted proxies, or broken by an entry that is not an address falls back to
// the socket address.
//
// The result is the canonical text of a parsed address, or "" when nothing
// usable was found. It is never a raw header fragment.
func Resolve(trusted bool, peer string, forwarded []string, ranges Ranges) string {
	if trusted {
		for i := len(forwarded) - 1; i >= 0; i-- {
			entry := strings.TrimSpace(forwarded[i])
			if entry == "" {
				continue
			}
			addr, ok := parseAddr(entry)
			if !ok {
				break
			}
			if ranges.Contains(addr) {
				continue
			}
			return addr.String()
		}
	}
	if addr, ok := parseAddr(strings.TrimSpace(peer)); ok {
		return addr.String()
	}
	return ""
}

// FromCtx resolves the client address of an in-flight request. The trust
// decision is Fiber's own, so a handler and c.IP() can never disagree about
// which peers are proxies.
func FromCtx(c fiber.Ctx, ranges Ranges) string {
	return Resolve(c.IsProxyTrusted(), peerAddress(c), Forwarded(c), ranges)
}

// Forwarded returns the `X-Forwarded-For` chain in header order. Repeated
// header lines are read as one list, as RFC 9110 requires, so a hop that writes
// a second line instead of extending the first cannot hide the end of the
// chain.
func Forwarded(c fiber.Ctx) []string {
	request := c.Request()
	if request == nil {
		return nil
	}
	lines := request.Header.PeekAll(fiber.HeaderXForwardedFor)
	entries := make([]string, 0, len(lines))
	for _, line := range lines {
		for _, entry := range strings.Split(string(line), ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

func peerAddress(c fiber.Ctx) string {
	request := c.RequestCtx()
	if request == nil {
		return ""
	}
	if ip := request.RemoteIP(); ip != nil {
		return ip.String()
	}
	return ""
}

// parseAddr accepts the spellings a proxy may put in the chain: a bare address,
// an address with a port, and the bracketed IPv6 forms of both. An IPv4-mapped
// IPv6 address collapses to its IPv4 form so one client is one value. Anything
// else, including an address carrying a zone, is rejected.
func parseAddr(raw string) (netip.Addr, bool) {
	if raw == "" {
		return netip.Addr{}, false
	}
	if addr, err := netip.ParseAddr(raw); err == nil {
		return canonical(addr)
	}
	if addrPort, err := netip.ParseAddrPort(raw); err == nil {
		return canonical(addrPort.Addr())
	}
	if len(raw) > 2 && raw[0] == '[' && raw[len(raw)-1] == ']' {
		if addr, err := netip.ParseAddr(raw[1 : len(raw)-1]); err == nil {
			return canonical(addr)
		}
	}
	return netip.Addr{}, false
}

func canonical(addr netip.Addr) (netip.Addr, bool) {
	if !addr.IsValid() || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func mustParseRanges(raw string) Ranges {
	ranges, err := ParseRanges(raw)
	if err != nil {
		panic(err)
	}
	return ranges
}
