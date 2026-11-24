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
every Group id Keycloak no longer has. Each such Group is read again by its
id, and only a "not found" deletes its rows; any other answer stops the run
with nothing deleted. A run deletes nothing, and counts it in
`skylab_communication_preference_team_cleanup_refused_total{reason}`, on a
list it cannot trust: a list without `/UYELER` or one that missed a branch
(a Group's children answered 404 while it was read, or a Group the list did
not hold is still there by its id: `incomplete_list`), or more teams gone
than a quarter of those with rows, and more than five (`too_many_gone`, as
for deleted people below). The next run tries again. Core’s [empty-leaf Group deletion](group-deletion.md) also
leaves preference cleanup to this job; the tree stops offering a deleted Group
within its existing one-minute cache lifetime. With Integration events on
(`EVENTS_ENABLED=true`), Keycloak's admin `GROUP DELETE` on `core.keycloak`
wakes this part of the job at once
([integration-events.md](integration-events.md#corekeycloak-keycloaks-events)):
it reads the whole Group list as the daily run does, with the same checks,
so a deleted Group's sub-Groups, which get no event of their own, go too.
The first event runs it at once; events that come before the next run make
one run, and two runs are at least 30 seconds apart. The
daily run stays.

**Deleted people (D2).** In the same daily run, core asks Keycloak for
every person who has rows, here or in App preferences
([app-preferences.md](app-preferences.md)), and deletes the rows of those
Keycloak answers "not found", in both tables: an account deleted straight
from the Keycloak console, outside the erasure saga, keeps no preference. Any other Keycloak error stops the
run with nothing deleted, and so does a run that finds more than a quarter
of the people (and more than five) gone, which a deletion outside the saga
does not explain (Keycloak answering for another realm, say). It is one
Keycloak read per person with rows, once a day. With Integration events on,
Keycloak's admin `USER DELETE` and account-console `DELETE_ACCOUNT` remove a
person's rows within seconds instead: when core has no erasure request for
the person (that is the saga's, which deletes them in its own step) and
Keycloak answers "not found" for them, the rows (choices, a waiting tracking
withdrawal, the tracking audit trail, App preferences) go in the event's transaction, with the
warning `keycloak_user_deleted_outside_erasure`. The daily run stays.

## API

All bodies are JSON. Errors are `application/problem+json` with a `code`:
`communication_preference_invalid` (400),
`communication_preference_team_unknown` (400),
`communication_preference_person_only` (403),
`communication_preference_subject_inactive` (403),
`communication_preference_follow_members_only` (403),
`communication_preferences_unavailable` (503,
no link key set),
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
- A token that does not read (changed, cut, another key, a key version not
  in the ring) is `400` (page: "Bağlantı geçersiz"; one-click:
  `invalid link`). No key set is `503`.
- The pages answer with `Cache-Control: no-store`,
  `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY` and a CSP without
  scripts, as the consent pages do. Each client address may open the page
  120 times a minute and POST 600 times a minute (one-click POSTs of a mail
  provider share a few addresses).

**The token.** Unpadded base64url of: the format and, from format 2, the
key version (below), kind (`u`), the channel (a tag; `email` is 1, a tag is
never reused), the person's id (16 bytes), the number of categories (1 to
32) and each category (a tag; a team's Group id with its length), then the
first 16 bytes of an HMAC-SHA256 over all of it under a key derived, for
this purpose only, from the key version that signed it. It carries no
address and does not expire (a mail may be opened years later). Core never
logs it: the error handler logs the path without the query, and these
routes answer pages, not problem documents (which echo the query). The
edge proxy's access log must not keep query strings of this path either.

**The key ring** (ADR 0072 Consequences; hub ticket abonelik-cikisi 13, the
same rules as SkyMail's unsubscribe key ring). A link names the key version
that signed it, so a new key does not break the links already mailed:

| Format | Starts with | Signed by |
|---|---|---|
| 1 | `1` | version 1, `COMMUNICATION_PREFERENCE_KEY`. Every link mailed before the ring is one |
| 2 | `2`, then the version (2–255) | `COMMUNICATION_PREFERENCE_KEY_V<n>` |

- New links are signed with `COMMUNICATION_PREFERENCE_KEY_CURRENT`. Unset
  or `1`, core signs format 1, as before the ring: an image from before the
  ring (which reads only format 1) can be rolled back without breaking a
  link. Format 2 appears only after the first new version.
- Every version set reads its own links; the ring is exactly the versions
  set. Format 2 never names version 1.
- **Planned change:** a new `V<n>` is added and `CURRENT` moves to it. The
  old version stays in the ring **for good**: its links keep working (RFC
  8058; CAN-SPAM's at least 30 days).
- **Leak:** the leaked version's variable is removed **at once**, a new
  version becomes `CURRENT`, and core is deployed. Links of that version no
  longer read (`400`); the person turns the category off on the Account
  center instead. Whoever holds a leaked key could otherwise sign a link
  that turns off someone else's categories. Version 1 leaked is
  `COMMUNICATION_PREFERENCE_KEY` removed.
- A version number is never given to another key, and two versions never
  hold the same key.
- The nightly secret rotator never turns these keys: a new version is
  written only by the hub's key wizard, the old ones kept.

## Tracking

SkyMail can record whether a person opened a mail and which link they
clicked (open and click tracking, ADR-0074; skymail-backend README "Açılma
ve tıklama takibi"; hub tickets iletisim-servisi 14 and 16). SkyMail's
flags decide which mail is tracked (`SKYMAIL_TRACKING`,
`SKYMAIL_TRACKING_OPERATOR`, `SKYMAIL_TRACKING_SYSTEM`).

**Every mail is tracked (B9, ADR-0074 addendum of 2026-11-24).** A person's
objection, not being a Member and having no account no longer stop
tracking. Core says no only for an inactive account, and for everyone while
the operator's brake is on. The Account center has no tracking switch any
more.

**The mode.** `COMMUNICATION_TRACKING_MODE` is the operator's switch, not a
personal preference:

| Mode | Core lets SkyMail track |
|---|---|
| `opt_out` (default, unset) | every active account |
| `opt_in` | no one (the brake; reason `tracking_off`) |

Any other value stops core at startup; the names are the ones from before
B9, kept so a deployment's value still reads. Startup logs the mode, with a
`WARNING` while the brake is on.

**The screen (one release of compatibility).** `GET` and `PUT
/v1/users/me/communication-preferences` still answer `"tracking":
{"enabled", "effective", "mode"}` so an older Account center keeps working.
`enabled` and `effective` are both the mode's (`true` under `opt_out`,
`false` under `opt_in`), whatever the person's old row says, Member or not:
the signed-in person is active, so this is the truth about them. A `PUT`
still takes `"tracking": {"enabled": true|false}` (anything else `400`, as
before) and ignores it: no row, no audit row, no withdrawal. A `PUT` that
carries only `tracking` reads the screen as `GET` does; while Keycloak does
not answer it is `200` with `"partial": true` and no team or News switch. A
`PUT` with `items` writes them as before. The field and the `PUT` key go in
a later release.

**Whom SkyMail may track.** SkyMail asks before it queues a tracked send:

```
POST /internal/v1/communication-preferences/tracking:resolve
Authorization: Bearer <SkyMail's service account token>

{"subs": ["7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10", …]}
```

→ `200 {"results": [{"sub", "allowed": true|false, "reason": "inactive"|"tracking_off"|null}]}`,
every `sub` asked exactly once, in the order asked (`Cache-Control:
no-store`). At most 1000 subs, each a lower-case hyphenated UUID, once;
`{"subs": []}` answers `[]`. The first rule that fails is the reason:

1. `tracking_off`: the brake (`opt_in`), for everyone. It reads neither
   Keycloak nor the accounts, so it holds while Keycloak does not answer;
2. `inactive`: the erased subject, an account core holds as being erased or
   erased, or a club member Keycloak has disabled (the snapshot holds the
   club's members only, so a disabled non-Member is not seen and is
   allowed; known case, kept on purpose).

Before B9 core also said `preference_off` and `not_member`; it no longer
does. It reads the News audience's snapshot of the `/UYELER` tree (30 s)
and needs no `COMMUNICATION_PREFERENCE_KEY`. Only a service account with
core's client role `communication-preferences:read` (SkyMail's,
`skymail-backend`) may call it: a person's token is `403
tracking_resolve_service_only` whatever it carries, a service account
without the role `403 tracking_resolve_role_missing`. A body core cannot
read is `400 tracking_resolve_invalid`; Keycloak not answering the tree is
`503 tracking_resolve_directory_unavailable` with `Retry-After`. The log
line `tracking_resolve` carries counts (`inactive`, `tracking_off`), never a
sub.

**Internal routes.** Every route under `/internal` answers a request that
came through the public edge (one carrying `X-Forwarded-*`, `Forwarded` or
`X-Real-Ip`) a bare `404` before its token is read, as SkyMail's do. It is
a second layer only: any container on the network reaches core without
Traefik, so the token checks stay the boundary.

**What was kept from before B9.** Nothing is deleted by this change:

- the old rows (`communication_preferences`, `scope = 'tracking'`) stay and
  are read by nothing;
- the audit trail `communication_tracking_changes(user_id, enabled,
  previous, source, changed_at)` gets no new row; its rows are still
  deleted 3 years after their change by the retention sweep's
  `tracking_changes_age` ([retention-sweep.md](retention-sweep.md));
- the outbox `communication_tracking_withdrawals` gets no new row, and the
  worker that sent its rows to SkyMail (`PUT
  /internal/v1/tracking-withdrawals/{sub}`) is no longer started:
  `COMMUNICATION_TRACKING_WITHDRAWAL_URL` is not read and the
  `skylab_tracking_withdrawal_*` metrics are gone. A row still waiting
  there stays. SkyMail's route answers success for one release and deletes
  nothing.

**Erasure.** The step `erase_communication_preferences` still deletes the
person's old row, their waiting withdrawal and their audit trail; SkyMail
deletes its tracking data in its own erasure step
([account-erasure-command.md](account-erasure-command.md)).

## Settings

| Variable | |
|---|---|
| `COMMUNICATION_PREFERENCE_KEY` | key version 1: 32 random bytes, unpadded base64url (OpenBao reference, set by a wizard). Its own key, not `CONTACT_CONSENT_KEY` (which stays unset in production until the retention sweep runs). Never changed in place: a new key is a new version ([the key ring](#the-unsubscribe-link)) |
| `COMMUNICATION_PREFERENCE_KEY_V<n>` | key version `n` (2–255), the same form, each its own OpenBao reference. `COMMUNICATION_PREFERENCE_KEY_V1` is not read: version 1 is the variable above. No version set at all: off, every route above `503` |
| `COMMUNICATION_PREFERENCE_KEY_CURRENT` | the version new links are signed with, 1–255; unset: 1. It must name a version that is set |
| `PUBLIC_API_ORIGIN` | required with the key: where the links point |
| `COMMUNICATION_PREFERENCE_ACCOUNT_CENTER_URL` | the Account center page the unsubscribe page links to; unset: `https://my.yildizskylab.com` |
| `COMMUNICATION_TRACKING_MODE` | `opt_out` (unset: every active account is tracked) or `opt_in` (the brake: no one is) ([Tracking](#tracking)); anything else stops core |
| `COMMUNICATION_TRACKING_WITHDRAWAL_URL` | Not read since B9 (the withdrawal worker is not started, [Tracking](#tracking)); a value left set does nothing |

A current version that is not set, a newer version without `CURRENT`, a key
that is not 32 bytes, `_V1`, or two versions holding the same key stop core
at startup (all of core, not only the preferences: a configuration error,
fail fast); the error names the variable, never a value. Core never falls
back to another version.

Startup logs `communication preferences: on|off` with the key versions and
the current one (never a key), and `communication tracking: mode …`
(a `WARNING` while the brake is on).
