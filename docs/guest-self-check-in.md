# Guest self check-in, the door QR and the rotating Session QR

A guest (a Ticket written by Guest apply or a form report, no account) checks themselves in to
a Session with `POST /v1/sessions/{sessionId}/check-in/guest`. The route takes
no sign-in. Until now it took only an e-mail, and Session ids are public
(`GET /v1/event-days/{id}/sessions`), so anybody anywhere could write an
attendance for any guest e-mail with a Ticket on that Event. Attendance turns
into certificates (ADR-0039).

The door QR closes that: a short-lived token, signed by core, shown on a
screen at the door. Scanning it is the proof that the guest is at the door.
Decision: Yusuf, 2026-10-03 (`core-internal-auth` spec, question 3).

Members' own check-in (`POST /v1/sessions/{id}/check-in/me`, with their
token) had the same hole (U12): the Session QR was a static image of the
Session's API address, so a photo of it sent home wrote an attendance, and a
certificate after it. The same signed token now closes that too (ADR-0082
decision 10): the Session QR is the door QR, shown on the projector or OBS
and rotated every 15 seconds, and `check-in/me` takes it as `doorToken`.
`SESSION_QR_MODE=qr` retires the static QR (below).

## Callers

Read on 2026-10-03 (GitHub code search over the organisation, and
`origin/main` of every local clone): nobody calls `check-in/guest`. sky-app's
Session QR page calls `check-in/me`; its door scanner calls
`check-in/skypass`; core-frontend calls `check-in/resolve` and `check-ins`.
So `qr` mode refuses no caller that exists today.

## Routes

### `POST /v1/sessions/{sessionId}/door-qr`

For the door screen and the attendance screen (projector, OBS). Requires a
token, and the caller must be allowed to take check-ins at the Session's
Event: the same rule as `check-in/resolve` and `check-ins` (door staff of the
Event, leaders of the owning team, Privileged, the team's members when
`team_door_scan=true`). No body.

`?use=door` (the default) mints a door token: guests pass the door one by
one, so it admits `DOOR_QR_MAX_USES` (20). `?use=display` mints the token
for a screen a whole room scans at once: it admits `ATTENDANCE_QR_MAX_USES`
(400). Anything else is `400`. `201`, `Cache-Control: no-store`:

```json
{
  "token": "v1.t3kq8g.AbCdEfGh.0123456789abcdef",
  "url": "https://api.yildizskylab.com/v1/sessions/<sessionId>?dq=v1.t3kq8g.AbCdEfGh.0123456789abcdef",
  "use": "door",
  "maxUses": 20,
  "sessionQrMode": "open",
  "sessionId": "<uuid>",
  "eventId": "<uuid>",
  "issuedAt": "2026-10-03T18:00:00Z",
  "expiresAt": "2026-10-03T18:01:00Z",
  "refreshAfterSeconds": 15
}
```

The screen encodes `url` in the QR and asks for a new one every
`refreshAfterSeconds`. `sessionQrMode` is `SESSION_QR_MODE`: in `qr` the
static Session QR (`GET /v1/sessions/{id}/qr`) checks nobody in any more, so
the panel stops offering it for download. With `?svg=1` the answer also carries `svg`: the QR of
`url` drawn by core with square modules and the standard four-module margin
(about 7 KB).

| Status | `code` | Meaning |
|---|---|---|
| 401 | | no token |
| 403 | | not door staff of this Event |
| 403 | `session_closed` | the Session is cancelled or outside its time window (below) |
| 404 | | no such Session |
| 429 | `door_qr_rate_limited` | over 30 mints a minute for this person; `Retry-After` |

`url` is `DOOR_QR_GUEST_URL` with `{sessionId}` filled in and `dq=<token>`
added. Unset, it is the Session's API address, the shape the static Session
QR already has, so sky-app's Session QR scanner reads it as that Session
(and ignores `dq`). A guest's phone camera needs a web page instead: the
guest page lives in skylab-site at `/kapi/{sessionId}?dq=…` (no sign-in,
skylab-site#40). `DOOR_QR_GUEST_URL`:

- sandbox: `https://sandbox.yildizskylab.com/kapi/{sessionId}` (set);
- production: `https://yildizskylab.com/kapi/{sessionId}`, set after the
  skylab-site production release that ships the page (unset until then).

### `POST /v1/sessions/{sessionId}/check-in/guest`

Body: `{"email": "…", "doorToken": "…"}` or `{"ticketKey": "…", "doorToken": "…"}`.
`doorToken` is the `dq` value. Despite the route's name it serves everyone
who lands on the page the QR opens (a phone's camera opens the page, not
sky-app):

- `ticketKey` is the Ticket's access key (the ticket page's `/bilet/{key}`,
  which the phone may keep): it finds any Ticket of the Session's Event, a
  member's too. Another Event's key, a renewed (old) key or an unknown one
  is 404.
- `email` finds a guest Ticket written for it first; failing that, the
  Ticket of the member whose account e-mail it is (primary, or school
  e-mail), on this Event. Case and spaces do not matter.
- With both, the key is used.
- The key and a member's e-mail need a valid `doorToken` **in every mode**
  (ticket 19): a key without one is `403 door_qr_required`; in `open` mode
  an e-mail without one finds a guest Ticket only, as before, and a
  member's is 404.

Accepted risks (Yusuf's ticket 19 decision, the guest path's own): someone
in the room, holding a fresh token, can type another person's school or
account e-mail and check them in; and 201/409 against 404 tells them whether
that address holds a Ticket of this Event. The token proves presence; the
route's per-address budget (10 failures a minute) bounds the guessing.
**Deviation from ticket 19:** the failed attempts are not counted per key
(nor per e-mail): only the per-address budget applies. A key is 128 bits of
random, so guessing one is not the risk; an e-mail is, and it is bounded as
a guest's is.

| Status | `code` | Meaning |
|---|---|---|
| 201 | | checked in |
| 400 | | neither an e-mail nor a ticket key |
| 403 | `door_qr_required` | a ticket key without a `doorToken`, in any mode |
| 403 | `session_closed` | the Session is cancelled or outside its time window |
| 403 | `door_qr_required` | `qr` mode and no `doorToken` |
| 403 | `door_qr_invalid` | not a door QR of this Session and Event (forged, another key, garbled, over 1 KB) |
| 403 | `door_qr_expired` | past `expiresAt`: scan the screen again |
| 403 | `door_qr_used_up` | this token has let `DOOR_QR_MAX_USES` guests in: scan the screen again |
| 404 | | no Ticket for this e-mail or key on the Session's Event, or no such Session. While the Event takes registrations the body carries `registrationUrl`: its form (`formUrl`), else its external registration link; the page shows it. Closed registration, no field |
| 409 | | already checked in to this Session |
| 429 | `guest_check_in_rate_limited` | this address failed 10 times this minute; `Retry-After` |

The door QR is checked before any Ticket is looked up, so in `qr` mode the
route no longer tells a stranger which e-mails are registered; nor does
`registrationUrl`, which the Event's public page shows anyway.

**Session window.** Guests check in, and door QRs are minted, only while the
Session is not cancelled and now is within
[`startTime` − grace, `endTime` + grace] (`DOOR_QR_SESSION_GRACE`, default
and maximum 12 h, so the door screen can be set up well ahead and still works
after an overrun; a lower value narrows it). Minting and check-in use the
same window, so the screen never shows a QR the check-in would refuse. A
Session without a start or an end is open on that side. A cancelled Session
is closed whatever the time. This applies in both modes.

### `POST /v1/sessions/{sessionId}/check-in/me`

A member's own check-in (sky-app "QR Okut"). Requires a token. Optional
body `{"doorToken": "…"}`, the `dq` value of the scanned door or attendance
QR; no body (sky-app's versions that read only the static Session QR send
none) is the same as no token.

| Status | `code` | Meaning |
|---|---|---|
| 201 | | checked in |
| 400 | | body is not JSON |
| 403 | `session_closed` | a token was sent, or `qr` mode, and the Session is outside its window or cancelled |
| 403 | `door_qr_required` | `SESSION_QR_MODE=qr` and no `doorToken` |
| 403 | `door_qr_invalid` / `door_qr_expired` / `door_qr_used_up` | as for guests: scan the screen again |
| 404 | | no Ticket for this person on the Session's Event, or no such Session |
| 409 | | already checked in to this Session |

Both classes of token are accepted, door and display, each against its own
cap. Only a check-in that happened (or found the member already checked in)
spends a use. Without a token in `open` mode nothing changes from before:
no Session window, so today's sky-app keeps working until the switch.

## Modes (`GUEST_SELF_CHECKIN_MODE`)

- `open` (default): an e-mail alone is enough. A `doorToken` that is sent is
  still checked, so a door UI can be tested before the switch.
- `qr`: every guest check-in needs a valid `doorToken`.

Any other value stops core at start-up, so a typo cannot reopen the route.

## Session QR mode (`SESSION_QR_MODE`)

The same two values for `check-in/me`:

- `open` (default): no token needed, so the static Session QR works. A
  `doorToken` that is sent is checked, and with one the Session window
  applies.
- `qr`: every member check-in needs a valid door or attendance token. A
  printed or photographed static Session QR no longer writes attendance.

A typo stops core at start-up. Start-up logs
`guest self check-in: GUEST_SELF_CHECKIN_MODE=…; session qr: SESSION_QR_MODE=…`.

## The token

`v1.<exp>.<jti>.<mac>`, about 35 characters, so the QR of the whole URL is
45×45 modules (version 7, error correction M) instead of the 77×77 a JWT
needed:

- `exp`: expiry, Unix seconds in base 36;
- `jti`: 6 random bytes, unpadded base64url;
- `mac`: HMAC-SHA256 over a domain label, the Session id, the Event id, `exp`
  and `jti`, truncated to 12 bytes (96 bits), unpadded base64url.

The Session id comes from the URL path and the Event id is never sent: both
are only bound into the MAC. Check-in therefore verifies the token against
the path's Session and the Event that Session belongs to now (a Session moved
to another Event's day loses its tokens). There is no algorithm field, so no
algorithm confusion: anything that is not exactly this shape, JWTs included,
is refused. A token over 1 KB is refused before it is parsed.

A display token carries one more field, `v1.<exp>.<jti>.a.<mac>` (about 37
characters). Its MAC uses its own domain label, so a door token cannot be
relabelled as a display token to get the larger use cap, nor the reverse;
every replica reads the class from the token itself.

Times: `exp` = issue + `DOOR_QR_TTL` (default 60 s, allowed 30 s–10 min). A
token is expired at `exp`; one whose `exp` lies more than TTL + 5 s ahead
(a clock far ahead, another configuration) is refused as invalid.

**Key.** Only core mints and reads these tokens, so the key is symmetric and
never published. It is derived from the SkyPass signing key with HKDF-SHA256
(RFC 5869, info `skylab core door-qr v1`): no new secret to provision, and
the SkyPass key itself never signs a door QR. The SkyPass key is
`SKYPASS_EC_PRIVATE_KEY`, or a P-256 key derived from the legacy
`SKYPASS_RSA_PRIVATE_KEY` when only that is set. If neither is set, core
does not start; only `SKYPASS_EPHEMERAL_KEY=true` (development) makes it
sign with a random key made at start-up, and then a restart invalidates the
door QRs on screen until their next refresh. Production and sandbox carry the
key as an OpenBao reference (`ops/wizards/skypass-signing-key-wizard.sh` in
the platform hub); it is never rotated automatically.

**More than one replica** accepts the same door QR only if every replica has
the same SkyPass key: set it explicitly wherever core runs more than once.
Rotating the SkyPass key rotates this key too; open door screens fetch a new
token within `refreshAfterSeconds`.

## Replay and abuse limits

A door QR is meant to be scanned by many guests at once, so it is not
single-use. What bounds a photo of it sent elsewhere:

- it expires after `DOOR_QR_TTL` (60 s), and the screen replaces it every
  quarter of that (15 s);
- the Session window: outside it nothing is accepted. At 12 h on each side
  it bounds the day of the Session rather than the hour, so the lifetime,
  the use cap and the fact that a QR exists only while door staff keep the
  screen open do the real work; narrow `DOOR_QR_SESSION_GRACE` if a door
  screen has to stay up unattended;
- each door token lets at most `DOOR_QR_MAX_USES` (default 20) guests in,
  each display token `ATTENDANCE_QR_MAX_USES` (default 400) people, members
  and guests together. Only a
  check-in that happened (or found the guest already checked in) spends a
  use; an e-mail without a Ticket gives its use back, so junk e-mails cannot
  use a token up;
- failures are budgeted per client address instead: 10 a minute (anything
  answered 400 or above, already-checked-in included). Successful check-ins
  do not count, so a crowd behind one campus NAT is not refused;
- a Ticket checks in to a Session once (`409` after that).

The use counts live in each core process, bounded to 10 000 tokens (expired
ones are dropped; only genuine tokens are counted and minting is limited to
30 a minute per person). With more than one replica the cap is per replica
(N replicas admit up to N × the cap); a restart resets it.

## Metrics

On `/v1/metrics`, no Session, Event or person named:

- `skylab_guest_self_checkin_total{door_qr="absent|present",outcome=…}` with
  outcomes `checked_in`, `already_checked_in`, `not_found`, `bad_request`,
  `session_closed`, `door_qr_required`, `door_qr_invalid`, `door_qr_expired`,
  `door_qr_used_up`, `failed`;
- `skylab_guest_self_checkin_mode{mode="open|qr"}`;
- `skylab_session_self_checkin_total{door_qr="absent|present",outcome=…}`,
  the same outcomes for `check-in/me`. `door_qr="absent"` successes are apps
  still reading the static Session QR: each would be refused under `qr`;
- `skylab_session_self_checkin_mode{mode="open|qr"}`.

Requests refused by the per-address budget (429) do not reach the counters.

## Rollout

1. This change, `open`: nothing changes for any caller (Session window aside;
   nobody calls the route).
2. `GUEST_SELF_CHECKIN_MODE=qr` right after the release: nobody calls the
   route, so nothing breaks, and guests cannot check themselves in until the
   door screens exist, which is already the case in practice. Production
   has run `qr` since 2026-10-04.
3. The door screen ships (core-frontend#20; sky-app staff scanner) and the
   guest page (skylab-site#40, `/kapi/{sessionId}`, no sign-in); then
   `DOOR_QR_GUEST_URL` (values above).
   Contract: `sky_lab_genel/.scratch/core-internal-auth/door-qr-sozlesme.md`.

Rollback: `GUEST_SELF_CHECKIN_MODE=open`.

### Session QR (U12)

1. This change, `SESSION_QR_MODE=open`: nothing changes for today's sky-app.
   The panel shows the rotating attendance QR (`?use=display`) on the
   projector; the static download stays while the mode is `open`.
2. sky-app's "QR Okut" reads the rotating QR (`/kapi/{id}?dq=…`, and the API
   shape with `dq`) and sends `doorToken` (contract for Mobile Lab:
   `sky_lab_genel/.scratch/bilet-qr-ve-pdf/sky-app-sozlesme-yoklama-qr.md`).
3. When that version is in the stores and
   `skylab_session_self_checkin_total{door_qr="absent",outcome="checked_in"}`
   has stopped growing at events: `SESSION_QR_MODE=qr`. The old app then
   answers "Oturumun doğrulanamadı." to a static QR (it shows every 403 so).

Rollback: `SESSION_QR_MODE=open`.

Members on `/kapi` by account e-mail or ticket key and `registrationUrl` on
a 404 came with ticket 22's rest; the Venue entry a check-in writes with
ticket 21 ([venue-entry.md](venue-entry.md)). The page (skylab-site) sends
`ticketKey` and shows `registrationUrl` in its own release; until then
members already check in there with their account e-mail.

## Compared with common practice

Matches: short-lived, context-bound, MAC-protected tokens; truncated
HMAC-SHA256 with a 96-bit tag (NIST SP 800-107 allows truncation to 64 bits
and more); key separation by HKDF with a context label; rotating on-screen
codes, as in the rotating barcodes of mobile ticketing; per-address limits on
an unauthenticated route.

Deviations:

- Not a JWT (RFC 7519): a JWT made the QR too dense to scan from a screen,
  and nobody but core reads the token, so there is nothing to interoperate
  with. The format is versioned (`v1`).
- The token is a bearer token: someone at the door can forward it within its
  lifetime. Binding it to the guest's device (DPoP, RFC 9449) needs a client
  key the guest's browser does not have. The use cap, the short lifetime and
  the Session window bound the damage.
- The replay cap is in memory and per process, not in a shared store.
- The per-address budget is keyed on the client address the edge proxy
  reports; overlay peers can write their own (docs/client-ip-trust.md).
