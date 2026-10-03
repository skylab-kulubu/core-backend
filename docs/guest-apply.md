# Guest apply

`POST /v1/events/{eventId}/applications/guest` writes a guest Ticket for an
e-mail on an Event, or finds the one already there. Body:
`{"firstName", "lastName", "email", "phoneNumber"?}`; first name, last name and
e-mail are required.

The route is public (`api.` sends it to core) and takes a token without
requiring one. Event ids are public, so anybody may call it with any e-mail.
What a caller gets back therefore depends on who the token says they are,
never on where the request came from.

## Callers

Read on 2026-10-03 (`origin/main` and `origin/production` of every repository
that calls core):

| Caller | Path | Token | Reads from the answer |
|---|---|---|---|
| forms-backend (`CoreGuestApply`), a guest's form answer | internal network, `Services__Users__BaseUrl` | none | the status code only: 2xx or 409 is success, anything else is "Başvuru kaydedilemedi" and the answer is not saved |
| core-frontend Event hub, "Katılımcı ekle" → "Misafir kaydı yaz" | browser → `api.` | the operator's | nothing: the page reloads the roster |

sky-app, the site and the other products do not call it.

## Caller classes

Every request is put in one class before anything else:

| Class | Request |
|---|---|
| `anonymous_public` | no usable token, and the resolved client address ([client-ip-trust.md](client-ip-trust.md)) is outside `TRUSTED_PROXY_RANGES`: it came from the internet through the edge proxy |
| `anonymous_internal` | no usable token, from inside the trusted ranges: today the forms hop |
| `person` | a person's valid token |
| `service` | a service account's valid token |

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

Until the forms hop sends its service token (below) it is not trusted, so a
guest who sends the form again with another name keeps the first name. An
operator can correct it from the Event hub.

Errors are the same for every caller: `400` for a body without the required
fields, `404` for an unknown Event, `429` (below).

## Rate limits

Only `anonymous_public` requests are limited, by two fixed-window budgets kept
in the process's memory:

- **each client address:** 20 requests per 10 minutes;
- **each Event and e-mail**, from any address: 5 requests per hour. The key is a
  SHA-256 digest of the Event id and the normalized e-mail; the e-mail itself is
  not kept.

A refused request gets `429`, `application/problem+json` with code
`guest_apply_rate_limited`, and `Retry-After` (also as `retryAfterSeconds`).
The answer is the same for both budgets; for the second, `Retry-After` is the
whole window. The second budget sends no `X-RateLimit-*` headers, which would
tell a caller how many requests others sent for an e-mail.

The forms hop is not limited: every guest's form answer goes through it, and a
`429` there loses the answer. A token holder is not limited either.

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

The counters start at zero when the process starts. Each request also writes
one JSON log line:
`{"event":"guest_apply","correlation_id":…,"caller":…,"outcome":…,"status":…}`.
It never carries an e-mail, a name, a phone number, an address or the Event.

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
