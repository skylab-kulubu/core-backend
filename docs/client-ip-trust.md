# Client address trust

Core never reads a network address out of a header it did not have to believe.
In production the service is reached only through the Traefik container that
terminates TLS on the private overlay network. That proxy discards whatever
`X-Forwarded-For` the caller sent and writes its own, holding a single entry:
the address it accepted the connection from. It also sets `X-Real-IP`, which
Core ignores in favour of the chain.

Because the proxy is a container, its address inside the overlay network is
assigned when it starts and changes whenever it is recreated. Nothing may
hardcode it. Trust is configured as the networks the proxy can appear on.

## Configuration

- `TRUSTED_PROXY_RANGES` — comma-separated CIDR ranges, or bare addresses for a
  single host. Default:
  `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8,::1/128,fc00::/7`.

The list is parsed and validated before the service accepts traffic. An entry
that is neither a CIDR range nor an address stops startup instead of being
skipped, because a typo would silently either trust a spoofable header or stop
trusting a real proxy. The effective list is written to the startup log.

Set it to the address space the reverse proxy shares with Core and nothing
wider. Any peer outside it is treated as an ordinary client that happens to
have sent a header.

## How an address is chosen

1. If the peer that opened the connection is not in `TRUSTED_PROXY_RANGES`, the
   forwarded-for header is ignored completely and the socket address is used.
   Anybody can send that header, so outside the proxy it means nothing.
2. If the peer is trusted, the chain is read from the right — the end a proxy
   appends to — and the first entry that is not itself a trusted proxy is the
   address that proxy observed. Entries further left were supplied by whoever
   sits behind that hop.
3. A chain that is empty, made only of trusted proxies, or broken by an entry
   that is not an address falls back to the socket address.
4. The stored value is always a parsed address in canonical form. Text that is
   not an address is stored as an empty string, never as the text that arrived.

The leftmost entry is never used. It is the one end of the chain a caller
controls, so honouring it would let a visitor decide what `url_hits.ip` says
about them.

## What depends on it

- `GET /v1/go/{alias}` and `GET /v1/go/{alias}/{channel}` record the resolved address as `url_hits.ip`.
- The public certificate routes (`/c/{serial}`, `/v1/public/certificates/…`,
  `/v1/certificates/verify/…`) are rate limited on the same resolved address, so
  the budget follows one visitor instead of being shared by everyone behind the
  proxy.

Fiber's `TrustProxy`, `TrustProxyConfig` and `ProxyHeader` are configured from
the same list, so `c.IP()` and the resolution above can never disagree about
which peers are proxies.
