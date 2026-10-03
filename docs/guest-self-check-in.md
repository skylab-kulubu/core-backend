# Guest self check-in and the door QR

A guest (a Ticket written by Guest apply, no account) checks themselves in to
a Session with `POST /v1/sessions/{sessionId}/check-in/guest`. The route takes
no sign-in. Until now it took only an e-mail, and Session ids are public
(`GET /v1/event-days/{id}/sessions`), so anybody anywhere could write an
attendance for any guest e-mail with a Ticket on that Event. Attendance turns
into certificates (ADR-0039).

The door QR closes that: a short-lived token, signed by core, shown on a
screen at the door. Scanning it is the proof that the guest is at the door.
Decision: Yusuf, 2026-10-03 (`core-internal-auth` spec, question 3).

Members' own check-in (`POST /v1/sessions/{id}/check-in/me`, with their
token) does not change.

## Callers

Read on 2026-10-03 (`origin/main` of every repository with a local clone):
nobody calls `check-in/guest`. sky-app's Session QR page calls `check-in/me`;
its door scanner calls `check-in/skypass`; core-frontend calls
`check-in/resolve` and `check-ins`. So `qr` mode refuses no caller that
exists today.

## Routes

### `POST /v1/sessions/{sessionId}/door-qr`

For the door screen. Requires a token, and the caller must be allowed to
take check-ins at the Session's Event: the same rule as
`check-in/resolve` and `check-ins` (door staff of the Event, leaders of the
owning team, Privileged, the team's members when `team_door_scan=true`).
No body. `201`, `Cache-Control: no-store`:

```json
{
  "token": "eyJ…",
  "url": "https://api.yildizskylab.com/v1/sessions/<sessionId>?dq=eyJ…",
  "sessionId": "<uuid>",
  "eventId": "<uuid>",
  "issuedAt": "2026-10-03T18:00:00Z",
  "expiresAt": "2026-10-03T18:02:00Z",
  "refreshAfterSeconds": 30
}
```

The screen encodes `url` in the QR and asks for a new one every
`refreshAfterSeconds`. With `?svg=1` the answer also carries `svg`: the QR of
`url` drawn by core (no logo), for a screen without a QR library. Errors: `401` no token, `403` not door staff of this
Event, `404` no such Session.

`url` is `DOOR_QR_GUEST_URL` with `{sessionId}` filled in and `dq=<token>`
added. Unset, it is the Session's API address, the shape the static Session
QR already has, so sky-app's Session QR scanner reads it as that Session
(and ignores `dq`). A guest's phone camera needs a web page instead; set
`DOOR_QR_GUEST_URL` once that page exists.

### `POST /v1/sessions/{sessionId}/check-in/guest`

Body: `{"email": "…", "doorToken": "…"}`. `doorToken` is the `dq` value.

| Status | `code` | Meaning |
|---|---|---|
| 201 | | checked in |
| 400 | | no e-mail |
| 403 | `door_qr_required` | `qr` mode and no `doorToken` |
| 403 | `door_qr_invalid` | not a door QR of this Session (forged, another key, another Session or Event, garbled) |
| 403 | `door_qr_expired` | past `expiresAt`: scan the screen again |
| 403 | `door_qr_used_up` | this token reached `DOOR_QR_MAX_USES`: scan the screen again |
| 404 | | no guest Ticket for this e-mail on the Session's Event, or no such Session |
| 409 | | already checked in to this Session |

The door QR is checked before any Ticket is looked up, so in `qr` mode the
route no longer tells a stranger which e-mails are registered.

## Modes (`GUEST_SELF_CHECKIN_MODE`)

- `open` (default): an e-mail alone is enough, as before. A `doorToken` that
  is sent is still checked, so a door UI can ship and be tested before the
  switch.
- `qr`: every guest check-in needs a valid `doorToken`.

Any other value stops core at start-up, so a typo cannot reopen the route.

## The token

A compact JWT (RFC 7519), HS256:

- header `typ: door-qr+jwt`, `kid: dq1`; a token without both is refused, so
  no other HS256 token can pass as a door QR, and `none`/asymmetric
  algorithms are never accepted;
- claims `sid` (Session id), `eid` (Event id), `jti` (96 random bits), `iat`,
  `exp` (`iat` + `DOOR_QR_TTL`, default 120 s, allowed 30 s–10 min).

Check-in verifies the signature, `exp` (required), `iat` not in the future,
`sid` equal to the path's Session, and `eid` equal to the Event the Session
belongs to now (a Session moved to another Event's day loses its tokens).

**Key.** Only core mints and reads these tokens, so the key is symmetric and
never published. It is derived from the SkyPass signing key
(`SKYPASS_EC_PRIVATE_KEY`) with HKDF-SHA256 (RFC 5869, info
`skylab core door-qr v1`): no new secret to provision, every replica with the
same SkyPass key accepts the same door QR, and the SkyPass key itself never
signs one. Rotating the SkyPass key rotates this key too; open door screens
fetch a new token within `refreshAfterSeconds`. If the SkyPass key is unset,
core makes a random one at start-up (as it already does for SkyPass), and a
restart invalidates the door QRs on screen until their next refresh.

**Replay.** A door QR is meant to be scanned by many guests at once, so it is
not single-use. What bounds a photo of it sent elsewhere:

- it expires after `DOOR_QR_TTL`, and the screen replaces it every quarter of
  that;
- each token admits at most `DOOR_QR_MAX_USES` (default 50) guest check-in
  attempts, counted per `jti` in core's memory, whatever their outcome;
- a Ticket checks in to a Session once (`409` after that).

The use count lives in each core process. With more than one replica the cap
is per replica (N replicas admit up to N × the cap); a restart resets it.

## Metrics

On `/v1/metrics`, no Session, Event or person named:

- `skylab_guest_self_checkin_total{door_qr="absent|present",outcome=…}` with
  outcomes `checked_in`, `already_checked_in`, `not_found`, `bad_request`,
  `door_qr_required`, `door_qr_invalid`, `door_qr_expired`,
  `door_qr_used_up`, `failed`;
- `skylab_guest_self_checkin_mode{mode="open|qr"}`.

In `open` mode, `door_qr="absent"` check-ins are the guests `qr` would refuse.

## Rollout

1. This change, `open`: nothing changes for any caller.
2. The door screen ships (core-frontend; sky-app staff scanner), and a guest
   page if `DOOR_QR_GUEST_URL` should point at one. Contract:
   `sky_lab_genel/.scratch/core-internal-auth/door-qr-sozlesme.md`.
3. `GUEST_SELF_CHECKIN_MODE=qr`. Because nobody calls the route today, this
   can also be done right away; guests then cannot check themselves in until
   the screens exist, which is already the case in practice.

Rollback: `GUEST_SELF_CHECKIN_MODE=open`.

## Compared with common practice

Matches: signed, short-lived, audience-bound bearer tokens (RFC 7519/8725:
explicit algorithm allow-list, `typ` header for explicit typing, `exp`
required); key separation by HKDF with a context label; rotating on-screen
codes, as in the rotating barcodes of mobile ticketing.

Deviations:

- The token is a bearer token: someone at the door can forward it within its
  lifetime. Binding it to the guest's device (DPoP, RFC 9449) needs a client
  key the guest's browser does not have. The use cap and short lifetime bound
  the damage.
- The replay cap is in memory and per process, not in a shared store.
- No `aud`/`iss` claims: `typ`, the derived key and the `sid`/`eid` binding
  do that job, and only core reads the token.
