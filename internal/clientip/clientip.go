// Package clientip decides when a forwarded-for header may be believed and
// resolves the address of the party that actually reached the edge proxy.
//
// The service runs behind a Traefik container that terminates TLS on a private
// overlay network. Traefik discards whatever `X-Forwarded-For` the caller sent
// and writes its own, so the header is worth reading exactly when the peer that
// opened the connection is one of the proxies an operator listed in
// TRUSTED_PROXY_RANGES. Traefik's address on that network is assigned by the
// container runtime and changes whenever it is recreated, and every other
// container on the network sits in the same subnet, so the proxy is named by
// its host name (resolved through the network's DNS and looked up again as it
// moves) rather than by an address or a range.
package clientip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
)

// EnvKey is the environment variable that carries the trusted proxies.
const EnvKey = "TRUSTED_PROXY_RANGES"

// DefaultRanges trusts the edge proxy alone: Dokploy's Traefik container,
// by its name on dokploy-network, and the loopback (a proxy beside the
// process in development). Another container on the network is not a proxy
// unless the setting names it.
const DefaultRanges = "dokploy-traefik,127.0.0.0/8,::1/128"

// PrivateRanges is the address space a container network hands out. It was
// the default proxy list until 2026-11-22; it now only tells an internal
// caller from the internet (Internal), never whose header to believe.
const PrivateRanges = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8,::1/128,fc00::/7"

// How often the named proxies are looked up again, and the shortest gap
// between two lookups when an ignored header asks for one sooner.
var (
	refreshEvery = 10 * time.Second
	refreshGap   = 2 * time.Second
)

// Ranges is a validated set of proxy addresses, networks and host names.
//
// The zero value trusts nobody, which makes every request fall back to its
// socket address rather than to a header a client can write.
type Ranges struct {
	configured []string
	prefixes   []netip.Prefix
	state      *state
}

// state is shared by every copy of one parsed Ranges: the named proxies'
// addresses as last looked up, and the counter of ignored headers.
type state struct {
	names []string

	mu     sync.Mutex
	byName map[string][]netip.Addr
	failed map[string]bool
	last   time.Time

	addrs   atomic.Pointer[[]netip.Addr]
	ignored atomic.Uint64
	poke    chan struct{}
}

var (
	defaultRanges = mustParseRanges(DefaultRanges)
	privateRanges = mustParseRanges(PrivateRanges)
	hostName      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9_-]{0,62}))*$`)
)

// Default returns the parsed DefaultRanges.
func Default() Ranges { return defaultRanges }

// ParseRanges reads a comma-separated list of CIDR ranges, addresses (a
// single host) and host names. A name is a proxy whose address the
// container runtime assigns: it is looked up through DNS (Refresh, Watch),
// and trusted for the addresses it resolves to. An entry that is none of
// these is an error rather than a silently ignored line, because a typo
// here would quietly make the service read a spoofable header or stop
// reading a real one.
func ParseRanges(raw string) (Ranges, error) {
	ranges := Ranges{state: &state{byName: map[string][]netip.Addr{}, failed: map[string]bool{}, poke: make(chan struct{}, 1)}}
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			addr, addrErr := netip.ParseAddr(field)
			switch {
			case addrErr == nil && addr.Zone() == "":
				addr = addr.Unmap()
				prefix = netip.PrefixFrom(addr, addr.BitLen())
			case isHostName(field):
				name := strings.ToLower(field)
				if !slices.Contains(ranges.state.names, name) {
					ranges.state.names = append(ranges.state.names, name)
				}
				ranges.configured = append(ranges.configured, name)
				continue
			default:
				return Ranges{}, fmt.Errorf("%s: %q is neither a CIDR range, an IP address nor a host name", EnvKey, field)
			}
		}
		ranges.configured = append(ranges.configured, field)
		ranges.prefixes = append(ranges.prefixes, prefix.Masked())
	}
	if len(ranges.configured) == 0 {
		return Ranges{}, fmt.Errorf("%s must list at least one proxy", EnvKey)
	}
	return ranges, nil
}

// isHostName accepts a DNS name with at least one letter, so that a
// mistyped address or range (digits, dots, colons, a slash) is an error
// and never a name that silently resolves to nothing.
func isHostName(field string) bool {
	if len(field) > 253 || !hostName.MatchString(field) {
		return false
	}
	return strings.IndexFunc(field, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
	}) >= 0
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

// Entries returns the configured entries, host names included, in order.
func (r Ranges) Entries() []string { return slices.Clone(r.configured) }

// Names returns the host names among the entries.
func (r Ranges) Names() []string {
	if r.state == nil {
		return nil
	}
	return slices.Clone(r.state.names)
}

// Empty reports whether no proxy is trusted.
func (r Ranges) Empty() bool { return len(r.configured) == 0 }

// Contains reports whether addr is a trusted proxy: inside one of the
// networks, or an address a named proxy resolved to at its last lookup.
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
	if r.state != nil {
		if addrs := r.state.addrs.Load(); addrs != nil && slices.Contains(*addrs, addr) {
			return true
		}
	}
	return false
}

// Internal reports whether addr is a caller inside the container network
// rather than on the internet: a trusted proxy itself, or any private or
// loopback address. It decides limits only (Guest apply's internal hop),
// never whose header to believe.
func (r Ranges) Internal(addr netip.Addr) bool {
	return r.Contains(addr) || privateRanges.Contains(addr)
}

// String renders the configured entries for startup logs.
func (r Ranges) String() string { return strings.Join(r.configured, ",") }

// Lookup resolves a host name to its addresses.
type Lookup func(ctx context.Context, host string) ([]netip.Addr, error)

// DNSLookup resolves through the system resolver: on a Docker network, the
// network's own DNS, which answers a container's name and `tasks.<service>`
// (a Swarm service's running tasks) with their addresses on that network.
func DNSLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// Refresh looks the named proxies up once and trusts what they resolve to.
// A name the DNS no longer knows loses its addresses (they may be handed to
// another container); a lookup that fails otherwise keeps the last ones,
// so a DNS hiccup does not stop believing the proxy. logf hears each name
// whose answer changed: its count of addresses, or why it is unresolved.
func (r Ranges) Refresh(ctx context.Context, lookup Lookup, logf func(format string, args ...any)) {
	if r.state == nil || len(r.state.names) == 0 {
		return
	}
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = time.Now()
	for _, name := range s.names {
		addrs, err := lookup(ctx, name)
		var dnsErr *net.DNSError
		switch {
		case err == nil:
			for i := range addrs {
				addrs[i] = addrs[i].Unmap()
			}
			slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
			addrs = slices.Compact(addrs)
			if s.failed[name] || !slices.Equal(addrs, s.byName[name]) {
				logf("trusted proxy %s: %d address(es)", name, len(addrs))
			}
			s.byName[name], s.failed[name] = addrs, false
		case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
			if !s.failed[name] || len(s.byName[name]) > 0 {
				logf("trusted proxy %s: not resolved (%v); its header is not believed", name, err)
			}
			s.byName[name], s.failed[name] = nil, true
		default:
			if !s.failed[name] {
				logf("trusted proxy %s: lookup failed (%v); keeping %d address(es)", name, err, len(s.byName[name]))
			}
			s.failed[name] = true
		}
	}
	union := make([]netip.Addr, 0)
	for _, name := range s.names {
		union = append(union, s.byName[name]...)
	}
	s.addrs.Store(&union)
}

// Watch looks the named proxies up again every few seconds, and sooner
// (but not twice within a couple of seconds) when a header is ignored from
// a peer that is not trusted, which is how a recreated proxy first shows.
// The channel closes when ctx ends. Without names there is nothing to watch.
func (r Ranges) Watch(ctx context.Context, lookup Lookup, logf func(format string, args ...any)) <-chan struct{} {
	done := make(chan struct{})
	if r.state == nil || len(r.state.names) == 0 {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(refreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-r.state.poke:
				r.state.mu.Lock()
				soon := time.Since(r.state.last) < refreshGap
				r.state.mu.Unlock()
				if soon {
					continue
				}
			}
			lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			r.Refresh(lookupCtx, lookup, logf)
			cancel()
		}
	}()
	return done
}

// Prometheus renders the named proxies' resolved addresses and the count
// of ignored headers for /v1/metrics.
func (r Ranges) Prometheus() string {
	var b strings.Builder
	b.WriteString("# HELP skylab_client_ip_forwarded_ignored_total Requests whose X-Forwarded-For was ignored because the peer is not a trusted proxy.\n")
	b.WriteString("# TYPE skylab_client_ip_forwarded_ignored_total counter\n")
	var ignored uint64
	if r.state != nil {
		ignored = r.state.ignored.Load()
	}
	fmt.Fprintf(&b, "skylab_client_ip_forwarded_ignored_total %d\n", ignored)
	if r.state == nil || len(r.state.names) == 0 {
		return b.String()
	}
	b.WriteString("# HELP skylab_trusted_proxy_addresses Addresses each named trusted proxy resolved to (0: not resolved).\n")
	b.WriteString("# TYPE skylab_trusted_proxy_addresses gauge\n")
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	for _, name := range r.state.names {
		fmt.Fprintf(&b, "skylab_trusted_proxy_addresses{name=%q} %d\n", name, len(r.state.byName[name]))
	}
	return b.String()
}

// ignoredHeader counts a header from a peer that is not trusted and asks
// Watch for an early lookup.
func (r Ranges) ignoredHeader() {
	if r.state == nil {
		return
	}
	r.state.ignored.Add(1)
	select {
	case r.state.poke <- struct{}{}:
	default:
	}
}

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

// FromCtx resolves the client address of an in-flight request, once: the
// answer is kept on the request, so a limiter and its handler agree and an
// ignored header is counted once. Fiber itself believes no forwarded header
// (internal/httpx); c.IP() is the socket's address.
func FromCtx(c fiber.Ctx, ranges Ranges) string {
	if resolved, ok := c.Locals(resolvedKey{}).(string); ok {
		return resolved
	}
	peer := peerAddress(c)
	forwarded := Forwarded(c)
	trusted := false
	if addr, ok := parseAddr(peer); ok {
		trusted = ranges.Contains(addr)
	}
	if !trusted && len(forwarded) > 0 {
		ranges.ignoredHeader()
	}
	resolved := Resolve(trusted, peer, forwarded, ranges)
	c.Locals(resolvedKey{}, resolved)
	return resolved
}

type resolvedKey struct{}

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
