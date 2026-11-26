# Guest apply

`POST /v1/events/{eventId}/applications/guest` writes a guest Ticket for an
e-mail on an Event, or finds the one already there. Body:
`{"firstName", "lastName", "email", "phoneNumber"?, "consents"?}`; first name,
last name and e-mail are required. While the Event's applying is not open
(closed by its organizer, past its deadline, ended or cancelled) the answer is
`409`, `code: "registration_closed"`, except for an operator adding a guest
from the panel (who may update the Event or assign its Tickets)
([event-registration-closure.md](event-registration-closure.md)).

`consents` carries the boxes the guest ticked
([contact-consents.md](contact-consents.md)): `["event_invitations"]`, or
`[{"purpose":"event_invitations","textVersion":"davet-v2"}]` to
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

The route is served on `api.`, but not to the internet without a token. A
request without a token that came through the public ingress (Traefik adds
`X-Forwarded-*` or `X-Real-Ip` to everything it passes on), or whose address
is not internal, is answered a bare `404` with `Cache-Control: no-store`
before its body is read (a broken body or a `consents` field changes
nothing): the same answer as the internal routes' guard
(`middlewares.InternalRouteGuard`, ADR-0016). A token core cannot verify
(invalid, expired) gets `401` there instead, as on every route behind
Bearer, so the panel fetches a new token and sends the request again. Event
ids are public, and with Kontenjan a token-less caller could fill an Event
with made-up addresses and mail each of them (OWASP OAT-021, Denial of
Inventory).

A token is not enough either: with sign-up open anybody can hold one. A
token holder must be an operator of the Event (who may update it or assign
its Tickets, Privileged included: the people the panel offers "Katılımcı
ekle" to). Anybody else, a member who runs nothing or any service account
(the forms service's too, `ticket:forms` or not: it reports its answers
instead, [form-response-tickets.md](form-response-tickets.md)), gets `403`
before anything is written. On a draft who may not see it still gets `404`
first ([event-drafts.md](event-drafts.md)).

## Callers

Read on 2026-11-24 (`origin/main` and `origin/production` of every repository
that calls core):

| Caller | Path | Token | Reads from the answer |
|---|---|---|---|
| core-frontend Event hub, "Katılımcı ekle" → "Misafir kaydı yaz" | browser → `api.` (through the edge) | the operator's | nothing: the page reloads the roster |

forms-backend no longer calls it: since `refactor!: drop event knowledge from
forms` (2026-10-07, in its production since) it reports every answer to
`POST /v1/forms/{formId}/responses` with its service token
([form-response-tickets.md](form-response-tickets.md)), over
`Integrations__SkylabCore__BaseUrl`, core's internal address. sky-app, the
site and the other products do not call it. The archived superadmin copies
send the operator's token like the Event hub.

### The internal hop

A request without a token from a private address that sends no forwarding
header is still answered: the old forms hop, which called this route on the
internal network without a token. No caller is known today;
`skylab_guest_apply_anonymous_internal_total` shows whether one appears. Its
answer is the untrusted one below, and any container on the overlay network
can make it, so the address is never a reason to show or change anything.
Once that counter has stayed unchanged for seven days (the log lines cover
restarts), the hop can be refused too.

## Caller classes

Every request is put in one class before anything else:

| Class | Request | Answer |
|---|---|---|
| `anonymous_public` | no usable token, and either it carries a forwarding header (it came through the edge) or its resolved address ([client-ip-trust.md](client-ip-trust.md)) is not internal | `404`, nothing read |
| `anonymous_internal` | no usable token, no forwarding header, from a private address (the overlay network) or a trusted proxy | the untrusted answer |
| `person` | a person's valid token | an operator of the Event: the trusted answer; anybody else `403` |
| `service` | a service account's valid token | `403`, whatever its roles |

An invalid or expired token leaves the request anonymous: from the edge it
is `401` (counted `anonymous_public`, `refused`), on the internal network
the internal hop's answer. A valid token meets the account
access gate and the Group overage step as on every other route, so a blocked
account's token gets `401` and a Group overage token gets `503` while
Keycloak is unreachable.

## What the caller gets

| | An operator of the Event | The internal hop |
|---|---|---|
| Answer | `201` and the Ticket | `201` and `{"status":"applied"}`, the same for a new guest and an existing one: no id, name, e-mail, phone number or check-ins |
| New e-mail on the Event | Ticket written | Ticket written |
| E-mail already has a guest Ticket | name and e-mail written; phone number written when sent | only a detail the Ticket lacks is filled; a different name or phone number is ignored and the stored one kept |

Two applications for one new e-mail at the same moment (a double submit)
write one Ticket: the one that loses the race finds the other's and is
answered as for an existing guest.

The form reports ([form-response-tickets.md](form-response-tickets.md)) are
not trusted either, so a guest who sends the form again with another name
keeps the first name. An operator can correct it from the Event hub.

Errors: `400` for a body without the required fields (`guest_fields_required`
with `fields`, the missing ones of `firstName`, `lastName`, `email`; a body
that is not JSON `invalid_body`, an Event id that is not one `invalid_id`;
[problem-codes.md](problem-codes.md)), `404` for an unknown
Event, `429` (below), for every caller that reaches the handler. A token
holder can also get `401` (a blocked account), `403` (not trusted, above) or
`503` (Group overage while Keycloak is unreachable, or the account access
gate unavailable).

## Rate limits

Two fixed-window budgets for each token subject (a valid token's `sub`),
kept in the process's memory:

| Budget | Size | Counts |
|---|---|---|
| ten minutes | 60 per 10 minutes | every request of the subject |
| daily ceiling | 300 per 24 hours | answered applications (2xx) only: a `403`, `400` or `409` does not use it |

The first is wide enough for an operator adding guests by hand; the second
caps what one operator's token, stolen or misused, can write in a day. A
request the first refuses does not count against the second. Each process
keeps its own count, so with several core replicas the ceiling is per
replica.

A refused request gets `429`, `application/problem+json` with code
`guest_apply_rate_limited`, and `Retry-After` (also as `retryAfterSeconds`).

The internal hop (`anonymous_internal`) is not limited, as before (it was the
forms hop, where a `429` lost a form answer). The address and e-mail budgets that limited
`anonymous_public`, and their switch `GUEST_APPLY_PUBLIC_IP_LIMIT_MODE`, are
gone with that class's access: a request they limited is now answered `404`
before any budget. The variable is no longer read.

## Counters and log

`/v1/metrics`:

- `skylab_guest_apply_requests_total{caller, outcome}`: `caller` is one of the
  four classes, `outcome` one of `created`, `existing` (the Ticket was there;
  what the caller may write was written), `kept` (the Ticket was there and a
  detail the caller may not change was ignored), `invalid`, `not_found`
  (an unknown Event, and an `anonymous_public` request without a token: the
  edge's `404`), `refused` (401/403), `rate_limited`, `failed`. Every pair is rendered, zero
  included; nothing else is ever a label value.
- `skylab_guest_apply_anonymous_public_total`,
  `skylab_guest_apply_anonymous_internal_total`,
  `skylab_guest_apply_person_total`, `skylab_guest_apply_service_total`: the
  same requests by class alone.
- `skylab_guest_apply_daily_cap_total`: requests the daily ceiling refused
  (also counted `rate_limited` above).

The counters start at zero when the process starts. Each request also writes
one JSON log line:
`{"event":"guest_apply","correlation_id":…,"caller":…,"outcome":…,"status":…}`.
It never carries an e-mail, a name, a phone number, an address or the Event.
