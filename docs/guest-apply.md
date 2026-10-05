# Guest apply

`POST /v1/events/{eventId}/applications/guest` writes a guest Ticket for an
e-mail on an Event, or finds the one already there. Body:
`{"firstName", "lastName", "email", "phoneNumber"?, "consents"?}`; first name,
last name and e-mail are required.

`consents` carries the boxes the guest ticked
([contact-consents.md](contact-consents.md)): `["event_invitations"]`, or
`[{"purpose":"event_invitations","textVersion":"davet-v1"}]` to
name the text shown. Absent or empty, nothing is recorded: the box is
unticked unless the guest ticked it. The field is judged whole before
anything is written, the guest's address included: an address a consent
cannot use, an unknown or not enabled purpose (`purpose_not_enabled`), an
unknown text, or more entries than purposes is `400` (`consent_invalid`,
`consent_text_unknown`) and no Ticket is written. After the Ticket each grant
is recorded `pending` and core mails the guest a confirmation link (no name
in it), whoever called: the answer below does not change. A grant that cannot
be recorded for a passing reason turns the `201` into `503`
(`consent_not_recorded`); sending the application again finds the Ticket and
records it.

The route is public (`api.` sends it to core) and takes a token without
requiring one. Event ids are public, so anybody may call it with any e-mail.
What a caller gets back therefore depends on who the token says they are,
never on where the request came from.

## Callers

Read on 2026-10-03 (`origin/main` and `origin/production` of every repository
that calls core):

| Caller | Path | Token | Reads from the answer |
|---|---|---|---|
| forms-backend (`CoreGuestApply`), a guest's form answer | internal network, `Services__Users__BaseUrl` (see **Where forms comes from**) | none | the status code only: 2xx or 409 is success, anything else is "Başvuru kaydedilemedi" and the answer is not saved |
| core-frontend Event hub, "Katılımcı ekle" → "Misafir kaydı yaz" | browser → `api.` | the operator's | nothing: the page reloads the roster |

sky-app, the site and the other products do not call it.

### Where forms comes from

The forms hop must reach core from inside `TRUSTED_PROXY_RANGES`, or it is
counted `anonymous_public` and every guest's form answer shares one address
budget. Checked on the production server on 2026-10-03:

- forms `Services__Users__BaseUrl` names core's internal Swarm service on
  port 8080, not a public host;
- core `TRUSTED_PROXY_RANGES=10.0.1.0/24`;
- the `dokploy-network` subnet is `10.0.1.0/24`.

So the hop is `anonymous_internal`. Check the three again whenever one of
them changes (a new forms deployment, a recreated network, a proxy range
change); `skylab_guest_apply_anonymous_internal_total` rising with each form
answer, and `anonymous_public` not, shows it from outside.

## Caller classes

Every request is put in one class before anything else:

| Class | Request |
|---|---|
| `anonymous_public` | no usable token, and the resolved client address ([client-ip-trust.md](client-ip-trust.md)) is outside `TRUSTED_PROXY_RANGES`: it came from the internet through the edge proxy |
| `anonymous_internal` | no usable token, from inside the trusted ranges: today the forms hop |
| `person` | a person's valid token |
| `service` | a service account's valid token. Only a service account whose client `MEDIA_SERVICE_CLIENTS` maps to a product is trusted (below); any other is counted here and answered like an anonymous caller |

An invalid or expired token is ignored, not refused: the request is
anonymous. A valid token meets the account access gate and the Group overage
step as on every other route, so a blocked account's token gets `401` and a
Group overage token gets `503` while Keycloak is unreachable.

The class decides only the rate limits and the counters below. The address is
never a reason to show or change anything: `anonymous_internal` is treated
exactly like `anonymous_public` apart from the limits, because any container
on the overlay network can look internal.

## What the caller gets

A caller is **trusted** when it is a product's service identity (a service
account whose client `MEDIA_SERVICE_CLIENTS` maps to a product) or an
operator of the Event: a person who may update the Event or assign its
Tickets, Privileged included. The Event hub offers "Katılımcı ekle" to exactly
those people.

| | Trusted | Anybody else |
|---|---|---|
| Answer | `201` and the Ticket, as before | `201` and `{"status":"applied"}`, the same for a new guest and an existing one: no id, name, e-mail, phone number or check-ins |
| New e-mail on the Event | Ticket written | Ticket written |
| E-mail already has a guest Ticket | name and e-mail written; phone number written when sent | only a detail the Ticket lacks is filled; a different name or phone number is ignored and the stored one kept |

Two applications for one new e-mail at the same moment (a double submit)
write one Ticket: the one that loses the race finds the other's and is
answered as for an existing guest.

Until the forms hop sends its service token (below) it is not trusted, so a
guest who sends the form again with another name keeps the first name. An
operator can correct it from the Event hub.

Errors: `400` for a body without the required fields, `404` for an unknown
Event, `429` (below), for every caller. A token holder can also get `401` (a
blocked account) or `503` (Group overage while Keycloak is unreachable, or the
account access gate unavailable).

## Rate limits

Three fixed-window budgets, kept in the process's memory:

| Budget | Applies to | Size | When it runs out |
|---|---|---|---|
| each token subject | `person` and `service` (a valid token's `sub`) | 60 per 10 minutes | 429 |
| each client address | `anonymous_public` | 20 per 10 minutes | 429 by default; see the switch below |
| each Event and e-mail, from any address | `anonymous_public` | 5 per hour | 429, always |

The subject budget is wide enough for an operator adding guests by hand and
keeps a member's token from writing fake guests without end. The Event and
e-mail key is a SHA-256 digest of the Event id and the normalized e-mail; the
e-mail itself is not kept.

A refused request gets `429`, `application/problem+json` with code
`guest_apply_rate_limited`, and `Retry-After` (also as `retryAfterSeconds`).
The body is the same for every budget, `Retry-After` is not: the subject and
address budgets give the seconds left in their window, the Event and e-mail
budget gives the whole window (3600), and sends no `X-RateLimit-*` headers,
which would tell a caller how many requests others sent for an e-mail.

The forms hop (`anonymous_internal`) is not limited: every guest's form
answer goes through it, and a `429` there loses the answer.

**`GUEST_APPLY_PUBLIC_IP_LIMIT_MODE`** sets the address budget:
`enforce` (the default, also when unset) refuses with 429; `observe` lets the
request through and counts it on
`skylab_guest_apply_public_ip_would_limit_total` (and `"would_limit":true` in
its log line). Any other value stops startup; the effective mode is in the
startup log (`guest apply per-address limit: …`). `observe` is the emergency
switch should a legitimate caller turn out to share one public address, for
example the forms hop reaching core through the edge after a configuration
change: set it in Dokploy and deploy, then fix the path and set it back.

An address on the overlay network can write any `X-Forwarded-For`
([client-ip-trust.md](client-ip-trust.md)), so the address budget does not
hold against a caller inside it; it is for the internet path.

## Counters and log

`/v1/metrics`:

- `skylab_guest_apply_requests_total{caller, outcome}`: `caller` is one of the
  four classes, `outcome` one of `created`, `existing` (the Ticket was there;
  what the caller may write was written), `kept` (the Ticket was there and a
  detail the caller may not change was ignored), `invalid`, `not_found`,
  `refused` (401/403), `rate_limited`, `failed`. Every pair is rendered, zero
  included; nothing else is ever a label value.
- `skylab_guest_apply_anonymous_public_total`,
  `skylab_guest_apply_anonymous_internal_total`,
  `skylab_guest_apply_person_total`, `skylab_guest_apply_service_total`: the
  same requests by class alone.
- `skylab_guest_apply_public_ip_would_limit_total`: requests the address
  budget would have refused while it observes (0 while it enforces).

The counters start at zero when the process starts. Each request also writes
one JSON log line:
`{"event":"guest_apply","correlation_id":…,"caller":…,"outcome":…,"status":…}`,
with `"would_limit":true` when the observing address budget would have refused
the request. It never carries an e-mail, a name, a phone number, an address or the Event.

## From log to enforce

1. **Now:** no request is refused for want of a token; the counters show who
   calls.
2. **Keycloak and forms:** a `core` client role for Guest apply is given to the
   forms service account, and forms sends its service token on this call as on
   its other core calls. Forms requests move from `anonymous_internal` to
   `service`, and forms can correct a guest's name again.
3. **Enforce:** a setting makes a request without a token `401`, a person who
   is not an operator `403`, and a service account without the role `403`.
   It is switched on in sandbox first, then in production once
   `skylab_guest_apply_anonymous_public_total` and
   `skylab_guest_apply_anonymous_internal_total` have stayed unchanged for
   seven days (the log lines cover restarts). Switching it off again is the
   way back.
