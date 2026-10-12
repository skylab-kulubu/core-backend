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
audience, [`news-audience.md`](news-audience.md)) are filtered by it.

Code: `internal/commpref`. Table: `communication_preferences`.

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
deletes nothing. Core has no route that deletes a Group, so this is the
only path.

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
  `{"items": [{"category": "team:<groupId>:duyuru", "channel": "email", "enabled": true|false|null}]}`
  (1 to 200 items, each category and channel once) → `200` and the new
  screen. `null` forgets the choice (back to what it inherits or its
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
only; the audience route (bilet 03) gives every recipient their own,
so SkyMail never holds the key.

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

## Settings

| Variable | |
|---|---|
| `COMMUNICATION_PREFERENCE_KEY` | 32 random bytes, unpadded base64url (OpenBao reference, set by a wizard). Unset: off, every route above `503`. Its own key, not `CONTACT_CONSENT_KEY` (which stays unset in production until the retention sweep runs). Never rotate: every mailed link depends on it |
| `PUBLIC_API_ORIGIN` | required with the key: where the links point |
| `COMMUNICATION_PREFERENCE_ACCOUNT_CENTER_URL` | the Account center page the unsubscribe page links to; unset: `https://my.yildizskylab.com` |

Startup logs `communication preferences: on|off`.
