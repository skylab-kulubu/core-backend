# Communication preferences

A communication preference is a person's choice of which News (duyuru,
haber) reaches them on which **channel**: the Duyuru tercihi of ADR 0067
(`skylab-kulubu/e-skylab`), kept per channel so the one communication
service (SkyMail growing into e-mail, in-app inbox, push and SMS; the
iletişim servisi decision, option B) reads one record. Today e-mail is the
only channel.

It is not a contact consent ([`contact-consents.md`](contact-consents.md)):
Member News runs on legitimate interest (ADR-0062), so club-wide News and a
person's own teams are on until they turn them off, and every such mail
carries a one-click link that turns its own categories off.

**Transactional messages never read it**, on any channel: sign-in codes,
tickets, certificates, team membership, Mail onayı and contact consent
confirmations go whatever the preferences say. Only News sends (bilet 03's
audience, [`news-audience.md`](news-audience.md)) and, since ADR 0072, every
duyuru-stream mail SkyMail sends to recipients a sender chose (the recipient
question, [`news-audience.md`](news-audience.md#recipients)) are filtered by
it. The recipient question counts every default as on: only the person's
own "off" holds such a mail back.

Code: `internal/commpref`. Table: `communication_preferences`. The open and
click tracking switch lives in the same table ([Tracking](#tracking)).

## Channels

| Channel | State |
|---|---|
| `email` | the only one today (the table's CHECK and `commpref.Channels`) |
| `inbox`, `push`, `sms` | not yet: each joins by widening the CHECK and the list together, when it ships |

Every switch below exists once per channel, the master switch included
("no push at all" is a different switch from "no e-mail at all"). The same
`channel` column and CHECK pattern as `contact_consents`.

## Categories

A category is a **source** times a **kind**, plus one master switch per
channel:

| Text form | Meaning |
|---|---|
| `all` | the channel's master switch: off, no duyuru or haber on that channel at all |
| `club:duyuru`, `club:haber` | club-wide mail (the "all Members" audience) |
| `team:<groupId>:duyuru`, `team:<groupId>:haber` | one team's mail; the team is its Keycloak Group id, so renaming or moving it keeps the choice |

A team is a Group `teammail.TeamOf` calls a team's own Group: at least
`/UYELER/<area>/<team>`, not a leader subgroup (`LIDERLER`,
`KOORDINATORLER`), not under a Privileged root.

## Values

Only a person's explicit choices are rows. The value of a category on a
channel is, computed on the Keycloak team tree of the moment and from that
channel's choices only:

1. the channel's master switch off: off (the other rows stay; turned on
   again, every earlier choice is back);
2. else, for a team, the most specific explicit choice of that kind, from
   the team up through the teams above it: a choice on a team covers its
   sub-teams, and a sub-team may be turned off on its own (Karar D5);
3. else the default:

| Source | Default |
|---|---|
| club-wide | on for a Member (anyone in `/UYELER`), off otherwise |
| a team the person is in, a team above one they are in (`TeamOf`'s subtree rule), and every team below those | on |
| any other team | off: following it is the person's own choice (Karar D7) |

The default inherits like a choice (Y1: a team's subscriber gets its
sub-teams' News). A WEBLAB member, also one who is in WEBLAB only through
its sub-team FRONT, gets the News of WEBLAB's sub-team BACK by default
(`origin: default`, `from: <WEBLAB's id>`), and may turn BACK off on its
own. An explicit choice on a team above beats a default below it: turning
WEBLAB off turns FRONT off too, unless FRONT has its own choice.

**Hidden teams (O2).** A team is *listed* when its own `public_listing` is
`true`, or, when it has none, when the nearest team above with one says
`true`; a team with `public_listing=false`, or with none anywhere above it,
is *hidden*. A hidden team is off for everyone who is not in it (or in a
team below it), whatever is chosen above it: a WEBLAB member's default, or
a follower's explicit choice on WEBLAB, does not reach a hidden
WEBLAB/PROJEX. Its own members get it on by default like any other team.
It is not shown to anyone else (below), and nobody else can turn it on or
off.

A team that is no longer in the tree is off. A moved sub-team takes its new
parent's choices and defaults and keeps its own choices.

**The tree** is read from Keycloak (one admin call per Group) at most once
a minute and shared by the screen, its changes and the unsubscribe page
(O1; concurrent reads share one). A renamed, moved, re-listed or deleted
team shows within that minute. The daily cleanups read Keycloak afresh.

## Rows

`communication_preferences(user_id, channel, scope, group_id, news_kind, enabled, source, updated_at)`,
one row per `(user_id, channel, scope, group_id, news_kind)` (a unique
constraint with `NULLS NOT DISTINCT`: the master switch and club rows have
NULLs).
`source` is where the choice was made: `account_center`,
`unsubscribe_link` or `admin` (no route writes `admin` yet). No address, no
name. `user_id` is the Keycloak subject; it needs no core `users` row (a
Member who never signed in to a core app can still use the link), but a
person being erased or erased gets no new row (a trigger refuses it under
the subject lock account deletion takes).

**Erasure.** Account erasure deletes every row of the person in its own
step, `erase_communication_preferences`, right after `erase_skypass_wallet` and
before the services ([account-lifecycle.md](account-lifecycle.md#durable-erasure-flow)).
No suppression list is kept (ADR-0051 decision 4). The step runs whether
preferences are on or off, and a repeat finds nothing.

**Deleted teams.** Once a day (and at startup), core deletes the rows of
every Group id Keycloak no longer has. A Group list without `/UYELER`
deletes nothing. Core’s [empty-leaf Group deletion](group-deletion.md) also
leaves preference cleanup to this job; the tree stops offering a deleted Group
within its existing one-minute cache lifetime.

**Deleted people (D2).** In the same daily run, core asks Keycloak for
every person who has rows and deletes the rows of those Keycloak answers
"not found": an account deleted straight from the Keycloak console, outside
the erasure saga, keeps no preference. Any other Keycloak error stops the
run with nothing deleted, and so does a run that finds more than a quarter
of the people (and more than five) gone, which a deletion outside the saga
does not explain (Keycloak answering for another realm, say). It is one
Keycloak read per person with rows, once a day.

## API

All bodies are JSON. Errors are `application/problem+json` with a `code`:
`communication_preference_invalid` (400),
`communication_preference_team_unknown` (400),
`communication_preference_person_only` (403),
`communication_preference_subject_inactive` (403),
`communication_preference_follow_members_only` (403),
`communication_preferences_unavailable` (503,
`COMMUNICATION_PREFERENCE_KEY` unset),
`communication_preference_teams_unavailable` (503 with `Retry-After`,
Keycloak did not answer the tree).

### A signed-in person

No sudo mode. A service account's token is refused (`403`).

- `GET /v1/users/me/communication-preferences` → the Account center's screen
  (`Cache-Control: no-store`):

  ```json
  {
    "channels": ["email"],
    "master": {"email": Cell},
    "club": {"duyuru": {"email": Cell}, "haber": {"email": Cell}},
    "teams": [TeamRow],
    "following": [TeamRow],
    "followable": [{"groupId", "code", "displayName", "path"}]
  }
  ```

  - Every switch is an object with one `Cell` per channel of `channels`,
    so the screen can show categories times channels; today only `email`.
  - `Cell` is `{"explicit": true|false|null, "enabled", "origin", "from"?, "effective"}`:
    `explicit` the person's own choice on this switch (null: none),
    `enabled` the switch's value without the channel's master switch (what
    the screen shows, greyed while the master switch is off), `origin` one
    of `explicit`, `inherited` (an explicit choice on the team `from`),
    `default` (with `from` when the default comes from the team above the
    person is in: "üst ekipten"), and `effective` whether the message goes.
  - `TeamRow` is `{"groupId", "code", "displayName", "path", "member", "kinds": {"duyuru": {"email": Cell}, "haber": {"email": Cell}}, "children": [TeamRow]}`.
  - `teams` are the trees of the top teams the person is in, with every
    sub-team they may see: a hidden sub-team they are not in is left out
    (O2).
  - `following` are the trees of the other teams the person made a choice
    on (on any channel), from the topmost one, hidden sub-teams left out.
    A hidden team the person is not in but has a choice on (left by an old
    link or a former membership) is shown **masked** (D1):
    `{"groupId", "hidden": true, "displayName": "Gizli ekip", "code": "", "path": ""}`,
    its switches off and no children, so the choice can be forgotten
    (`enabled: null`) and the screen names nothing.
  - `followable` are the publicly listed teams (`public_listing=true`) in
    neither list; `[]` for someone who is no Member (only a Member
    follows, below).
- `PUT /v1/users/me/communication-preferences`
  `{"items": [{"category": "team:<groupId>:duyuru", "channel": "email", "enabled": true|false|null}], "tracking": {"enabled": true|false}}`
  (0 to 200 items, each category and channel once, with `tracking` on top
  of the 200; `items`, `tracking` or both) → `200` and the new screen. `tracking` is the open and click
  tracking switch ([Tracking](#tracking)): only `true` or `false`, no
  `null`; `"tracking"` is no category of `items`. `null` forgets the choice (back to what it inherits or its
  default; "unfollow", "reset"). `channel` and `enabled` must be present; a
  channel core does not keep yet is `400`. Turning a team on **or off**
  needs the person to see it: they are in it, or it is listed (Karar D7,
  O2). A hidden team they are not in, a Privileged Group, an area and an id
  that does not exist all answer the same `400
  communication_preference_team_unknown`, so the answer tells nothing of a
  hidden team (D1). **Only a Member follows** (Karar D7): someone in no
  `/UYELER` Group (a guest, a Place user, a former Member) who turns a
  team **on** gets `403 communication_preference_follow_members_only`
  (after the unknown-team check, so it tells nothing of a hidden team); a
  non-Member subscriber is Faz 2's, on a Contact consent (SPEC §5.8, K8).
  Turning off and forgetting stay allowed. Rows written before this rule,
  and a former Member's, stay; the News audience leaves every follower who
  is no Member out ([news-audience.md](news-audience.md#candidates)).
  Forgetting is always allowed (a hidden or deleted team's row). All items
  are written in one transaction or none.

### The unsubscribe link

`GET|POST /v1/communication-preferences/unsubscribe?t=…`. No sign-in, no cookie, no
redirect: the token is the permission. `internal/commpref`
(`Service.UnsubscribeURL`) makes it for a person, the channel (`email`) and
the categories a mail was sent on, and every POST writes on that channel
only; the audience route (bilet 03) gives every recipient their own, and
the recipient question (`POST /internal/v1/audiences/recipients:resolve`,
ADR 0072, [news-audience.md](news-audience.md#recipients)) every allowed
person theirs for the one category asked, so SkyMail never holds the key.

- `GET` shows a page naming the categories ("AIRLAB · Duyurular") and
  changes nothing, so a mail scanner that opens the link turns nothing off.
  Its three buttons `POST` the token with `kapsam`: `categories` (only
  these), `sources` (both kinds of every source the link names: "bu
  kaynaktan her şeyi kapat"), `all` (the channel's master switch: "tümünden çık").
  It links to the Account center (`COMMUNICATION_PREFERENCE_ACCOUNT_CENTER_URL`)
  with the link's first category and channel,
  `?kanal=email&kategori=team%3A<groupId>%3Aduyuru`, so the screen can show
  that row first (bilet 09); the token never goes there, the person signs
  in. The confirmation page links the same way.
  When Keycloak does not answer, or a team is gone, the page names it
  "Bir ekip"; the link works the same.
- **One-click (RFC 8058).** Every News mail carries

  ```
  List-Unsubscribe: <https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=…>
  List-Unsubscribe-Post: List-Unsubscribe=One-Click
  ```

  The mail client `POST`s `List-Unsubscribe=One-Click` (form-encoded or
  multipart) to that address; core turns off exactly the link's categories
  (writes `enabled=false`, source `unsubscribe_link`) and answers `200 ok`,
  `no-store`. Both headers must be covered by the mail's DKIM signature.
- Repeating a POST answers as the first and leaves the same rows. A person
  being erased or erased writes nothing and is answered as done.
- A token that does not read (changed, cut, another key) is `400` (page:
  "Bağlantı geçersiz"; one-click: `invalid link`). The key unset is `503`.
- The pages answer with `Cache-Control: no-store`,
  `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY` and a CSP without
  scripts, as the consent pages do. Each client address may open the page
  120 times a minute and POST 600 times a minute (one-click POSTs of a mail
  provider share a few addresses).

**The token.** Unpadded base64url of: version (1), kind (`u`), the channel
(a tag; `email` is 1, a tag is never reused), the person's id (16 bytes), the number of categories (1 to 32) and each category (a tag;
a team's Group id with its length), then the first 16 bytes of an
HMAC-SHA256 over all of it under a key derived from `COMMUNICATION_PREFERENCE_KEY`
for this purpose only. It carries no address and does not expire (a mail
may be opened years later). Core never logs it: the error handler logs the
path without the query, and these routes answer pages, not problem
documents (which echo the query). The edge proxy's access log must not keep
query strings of this path either.

## Tracking

SkyMail can record whether a person opened a mail and which link they
clicked (open and click tracking, ADR-0074; skymail-backend README "Açılma
ve tıklama takibi"; hub tickets iletisim-servisi 14 and 16). Core decides
whom it may track, for every mail SkyMail tracks: panel sends
(`SKYMAIL_TRACKING_OPERATOR`) and the services' account and transactional
mail (`SKYMAIL_TRACKING_SYSTEM`) alike; SkyMail's flags decide which mail
is tracked, and they all start off. The basis is legitimate interest with
an opt-out (decision T1): a Member is tracked until they turn it off on the
Account center (T8: no mail carries a "stop tracking" link). The objection
covers account and transactional mail too; the mail keeps coming.

**The row.** `scope = 'tracking'`, `channel = 'email'`, no `group_id`, no
`news_kind`; one per person. `source` is `account_center` (or `admin`).
It is no News category: no link, audience or `items` entry names it, and
the master switch does not touch it.

**The mode.** `COMMUNICATION_TRACKING_MODE`:

| Mode | Without a row | `enabled=true` | `enabled=false` |
|---|---|---|---|
| `opt_out` (default, unset) | tracked | tracked | not tracked |
| `opt_in` | not tracked | tracked | not tracked |

Any other value stops core at startup. `opt_in` is the brake (everyone
without a row is out at once, SkyMail untouched) and the way back should
explicit consent be required.

**The screen.** `GET` and `PUT /v1/users/me/communication-preferences`
answer `"tracking": {"enabled", "effective", "mode"}`: `enabled` the
person's row or the mode's default; `effective` whether core lets SkyMail
track them now (enabled and a Member: someone in no `/UYELER` Group is
never tracked); `mode` the mode. The Account center shows the switch
reversed: "Postalarımın açılmasını ve tıklamalarımı izleme" checked is
`enabled: false`. Like every route above, it needs
`COMMUNICATION_PREFERENCE_KEY` (else `503`). A `PUT` that changes only
`tracking` reads nothing from Keycloak before it writes, so an objection is
kept while Keycloak is down: it answers `200` with `"partial": true`, the
switch as written (`effective` false until the screen is read again) and no
team or News switch. A `PUT` with `items` still needs the tree (`503
communication_preference_teams_unavailable`).

**Turning it off** writes, in one transaction: the row, a row of the audit
trail (below) when the value changes, and a withdrawal in the outbox
`communication_tracking_withdrawals` (one per person: a newer one replaces a
waiting one, due at once). A worker then sends

```
PUT {COMMUNICATION_TRACKING_WITHDRAWAL_URL}/internal/v1/tracking-withdrawals/{sub}
Authorization: Bearer <core's service account token, client core>
```

with no body; `{sub}` is the lower-case hyphenated UUID. SkyMail closes
every tracking token of the person's mail and deletes their openings and
clicks; their old mail's links still redirect. The token comes from
`KEYCLOAK_CLIENT_ID` (`core`) by client credentials and needs `aud ∋
skymail` and SkyMail's client role `skymail:tracking:withdraw` (given only
to `service-account-core`, hub ticket 17). The answers:

| SkyMail answers | Core |
|---|---|
| `204` | done, the row goes |
| no answer, `401` (the token is dropped), `429`, `5xx` (`503` with `Retry-After`) | tried again: 30 s doubling to 15 min, a `Retry-After` kept between the two |
| `403` | configuration error (role or audience missing): the token is dropped, logged `tracking_withdrawal_forbidden`, tried again every 15 min |
| `404` | no route (a SkyMail before 14c, or a public address): logged `tracking_withdrawal_not_found`, tried again every 15 min |
| `400` | refused for good: logged `tracking_withdrawal_rejected`, the row goes |

A withdrawal is never given up on: the objection must reach SkyMail. Core
ships before SkyMail's route without harm (it waits on `404`). Turning
tracking back on writes no withdrawal; what SkyMail deleted stays deleted.
Several cores share the outbox (claims with a lease, `FOR UPDATE SKIP
LOCKED`); a claim's late answer never removes a newer request.

**Whom SkyMail may track.** SkyMail asks before it queues a tracked send:

```
POST /internal/v1/communication-preferences/tracking:resolve
Authorization: Bearer <SkyMail's service account token>

{"subs": ["7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10", …]}
```

→ `200 {"results": [{"sub", "allowed": true|false, "reason": "preference_off"|"not_member"|"inactive"|null}]}`,
every `sub` asked exactly once, in the order asked (`Cache-Control:
no-store`). At most 1000 subs, each a lower-case hyphenated UUID, once;
`{"subs": []}` answers `[]`. The first rule that fails is the reason:

1. `inactive`: disabled in Keycloak, or being erased or erased in core;
2. `not_member`: in no `/UYELER` Group (the [News audience](news-audience.md)'s
   Member), a client's service account, or someone Keycloak does not have;
3. `preference_off`: the row is `false`, or there is none under `opt_in`.

It reads the News audience's snapshot of the `/UYELER` tree (30 s) and
needs no `COMMUNICATION_PREFERENCE_KEY`. Only a service account with core's
client role `communication-preferences:read` (SkyMail's,
`skymail-backend`) may call it: a person's token is `403
tracking_resolve_service_only` whatever it carries, a service account
without the role `403 tracking_resolve_role_missing`. A body core cannot
read is `400 tracking_resolve_invalid`; Keycloak not answering the tree is
`503 tracking_resolve_directory_unavailable` with `Retry-After` (SkyMail
then tracks nobody in that send). The log line `tracking_resolve` carries
counts, never a sub.

**Internal routes.** Every route under `/internal` answers a request that
came through the public edge (one carrying `X-Forwarded-*`, `Forwarded` or
`X-Real-Ip`) a bare `404` before its token is read, as SkyMail's do. It is
a second layer only: any container on the network reaches core without
Traefik, so the token checks stay the boundary.

**The audit trail.** `communication_tracking_changes(user_id, enabled,
previous, source, changed_at)`: a row whenever the value changes
(`previous` null when there was no row). No address, no IP. A row is kept
3 years after its change (ADR-0062 addendum of 2026-10-25), then the
retention sweep's `tracking_changes_age` deletes it
([retention-sweep.md](retention-sweep.md)); the preference row itself
stays.

**Erasure.** The step `erase_communication_preferences` deletes the
person's row, their waiting withdrawal and their audit trail; SkyMail
deletes its tracking data in its own erasure step
([account-erasure-command.md](account-erasure-command.md)).

**Metrics** (`/v1/metrics`, internal only): `skylab_tracking_withdrawal_enabled`,
`skylab_tracking_withdrawal_total{outcome="sent|retry|forbidden|not_found|rejected"}`,
`skylab_tracking_withdrawal_backlog`, `skylab_tracking_withdrawal_oldest_age_seconds`,
`skylab_tracking_withdrawal_last_success_timestamp_seconds`,
`skylab_tracking_withdrawal_queue_errors_total`. A backlog that ages is the
alarm.

## Settings

| Variable | |
|---|---|
| `COMMUNICATION_PREFERENCE_KEY` | 32 random bytes, unpadded base64url (OpenBao reference, set by a wizard). Unset: off, every route above `503`. Its own key, not `CONTACT_CONSENT_KEY` (which stays unset in production until the retention sweep runs). Never rotate: every mailed link depends on it |
| `PUBLIC_API_ORIGIN` | required with the key: where the links point |
| `COMMUNICATION_PREFERENCE_ACCOUNT_CENTER_URL` | the Account center page the unsubscribe page links to; unset: `https://my.yildizskylab.com` |
| `COMMUNICATION_TRACKING_MODE` | `opt_out` (unset) or `opt_in` ([Tracking](#tracking)); anything else stops core |
| `COMMUNICATION_TRACKING_WITHDRAWAL_URL` | SkyMail's address on the internal network for the withdrawal (not the public one: `/internal` answers the edge `404`); unset: `ACCOUNT_ERASURE_SKYMAIL_URL`. Neither set (or no `KEYCLOAK_*` client credentials): withdrawals wait in the outbox, startup warns. A value that does not read (either variable) does not stop core: startup warns and the withdrawals wait; the erasure worker still checks `ACCOUNT_ERASURE_SKYMAIL_URL` for itself |

Startup logs `communication preferences: on|off`, `communication tracking:
mode …` and whether withdrawals reach SkyMail.
