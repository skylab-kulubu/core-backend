# Client address trust

Core never reads a network address out of a header it did not have to believe.
In production the service is reached only through the Traefik container that
terminates TLS on the private overlay network. That proxy discards whatever
`X-Forwarded-For` the caller sent and writes its own, holding a single entry:
the address it accepted the connection from. It also sets `X-Real-IP`, which
Core ignores in favour of the chain.

Because the proxy is a container, its address inside the overlay network is
assigned when it starts and changes whenever it is recreated, and every other
container (the site, forms, SkyMail, …) sits in the same subnet. Nothing may
hardcode the address, and a range would trust the neighbours too. The proxy
is therefore named by its **host name**, which the network's DNS resolves.

## Configuration

- `TRUSTED_PROXY_RANGES` — comma-separated host names, CIDR ranges and bare
  addresses (a single host). Default: `dokploy-traefik,127.0.0.0/8,::1/128`
  (Dokploy's Traefik container, and the loopback for a proxy beside the
  process in development).

A **host name** is looked up through the system resolver: on a Docker
network the network's DNS, which answers a container's name
(`dokploy-traefik`) and `tasks.<service>` (a Swarm service's running tasks,
the addresses its connections come from; the bare service name is its
virtual IP, which no connection comes from). Core looks the names up before
the first request, every 10 seconds after, and at once (at most every
2 seconds) when a header arrives from a peer that is not trusted, which is how
a recreated proxy first shows. A name the DNS no longer knows loses its
addresses; a lookup that fails otherwise keeps the last ones. The log says
each name's count of addresses when it changes (never the addresses):
`trusted proxy dokploy-traefik: 1 address(es)`, or `… not resolved …; its
header is not believed`.

The list is parsed and validated before the service accepts traffic. An entry
that is neither a host name (letters required, so a mistyped address is not
taken for one), a CIDR range nor an address stops startup instead of being
skipped, because a typo would silently either trust a spoofable header or stop
trusting a real proxy. The configured list is written to the startup log.

List the reverse proxy and the containers that call Core on the internal
network on a visitor's behalf, and nothing wider. Any peer outside the list is
treated as an ordinary client that happens to have sent a header.

Production, once this is released (2026-11-22 decision):
`TRUSTED_PROXY_RANGES=dokploy-traefik,tasks.<the site's Dokploy appName>`.
Before, it was `10.0.1.0/24`, the whole `dokploy-network` subnet, which
believed any container there; that value still works and is the way back.

`/v1/metrics` (internal only) shows whether it holds:

- `skylab_trusted_proxy_addresses{name="…"}`: what each name resolved to at
  its last lookup; `0` means its header is not believed.
- `skylab_client_ip_forwarded_ignored_total`: requests whose
  `X-Forwarded-For` was ignored because the peer is not a trusted proxy.
  Rising with every public request means Traefik is not recognized (every
  visitor then counts as Traefik's own address and shares one budget):
  check the name, or set the old range back.

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
- The public ticket routes (`/v1/public/tickets/{key}`, its `/pdf` and its
  `/qr.png`) are rate limited on the resolved address the same way
  ([`ticket-page.md`](ticket-page.md)). The club site reads a ticket page
  from its own server: it calls Core on the internal network and names the
  visitor in `X-Forwarded-For`, which Core believes because the site's
  tasks are named in the list (`tasks.<appName>`). Through Traefik (the public address)
  the header would be replaced and every visitor would share the site's
  budget.
- `GET /v1/media/{id}/content` (a private Media's read link) records the
  resolved address of every open as `media_read_link_opens.client_ip`, kept a
  year, and is rate limited on it like the certificate routes (see
  [`media-lifecycle.md`](media-lifecycle.md#access-log)).
- Guest apply (`POST /v1/events/{eventId}/applications/guest`) limits
  token-less requests whose resolved address is public, per address, and
  counts token-less requests as internal (a private address: the overlay
  network, a trusted proxy) or public. The address decides neither what the
  caller sees nor what it may change ([`guest-apply.md`](guest-apply.md)).

Fiber believes no forwarded header (`TrustProxy` off, no `ProxyHeader`):
`c.IP()` is the socket's address, and only this resolution reads the chain.
It is worked out once per request, so a limiter and its handler agree.

## What it is not

The resolved address is not an identity. A named proxy is trusted for the
address the network's DNS gives it; a container that took over that address
between two lookups would be believed for those seconds. The address therefore serves only abuse limits and best-effort records
(`url_hits.ip`, `media_read_link_opens.client_ip`). No authorization decision
depends on it: a route that is not deliberately public asks for a token.
