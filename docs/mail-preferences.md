# Announcement preferences

An announcement preference (Duyuru tercihi, ADR 0067 in
`skylab-kulubu/e-skylab`) is a person's choice of which News mail
(duyuru, haber) SkyMail sends them. It is not a contact consent
([`contact-consents.md`](contact-consents.md)): Member mail runs on
legitimate interest (ADR-0062), so club-wide mail and a person's own teams
are on until they turn them off, and every such mail carries a one-click
link that turns its own categories off. Transactional mail (sign-in codes,
tickets, certificates, team membership, Mail onayı) never reads it.

Code: `internal/mailpref`. Table: `mail_preferences`.

## Categories

A category is a **source** times a **kind**, plus one master switch:

| Text form | Meaning |
|---|---|
| `all` | the master switch: off, no duyuru or haber mail at all |
| `club:duyuru`, `club:haber` | club-wide mail (the "all Members" audience) |
| `team:<groupId>:duyuru`, `team:<groupId>:haber` | one team's mail; the team is its Keycloak Group id, so renaming or moving it keeps the choice |

A team is a Group `teammail.TeamOf` calls a team's own Group: at least
`/UYELER/<area>/<team>`, not a leader subgroup (`LIDERLER`,
`KOORDINATORLER`), not under a Privileged root.

## Values

Only a person's explicit choices are rows. The value of a category is,
computed on the Keycloak team tree of the moment:

1. the master switch off: off (the other rows stay; turned on again, every
   earlier choice is back);
2. else, for a team, the most specific explicit choice of that kind, from
   the team up through the teams above it: a choice on a team covers its
   sub-teams, and a sub-team may be turned off on its own (Karar D5);
3. else the default:

| Source | Default |
|---|---|
| club-wide | on for a Member (anyone in `/UYELER`), off otherwise |
| a team the person is in, or a team above one they are in (`TeamOf`'s subtree rule) | on |
| any other team | off: following it is the person's own choice (Karar D7) |

A team that is no longer in the tree is off. A moved sub-team takes its new
parent's choices and keeps its own. Only an explicit choice inherits: a
person in WEBLAB (by default on) gets WEBLAB's sub-team BACK only if they
are in BACK or turn WEBLAB (or BACK) on explicitly.

## Rows

`mail_preferences(user_id, scope, group_id, news_kind, enabled, source, updated_at)`,
one row per `(user_id, scope, group_id, news_kind)` (a unique constraint
with `NULLS NOT DISTINCT`: the master switch and club rows have NULLs).
`source` is where the choice was made: `account_center`,
`unsubscribe_link` or `admin` (no route writes `admin` yet). No address, no
name. `user_id` is the Keycloak subject; it needs no core `users` row (a
Member who never signed in to a core app can still use the link), but a
person being erased or erased gets no new row (a trigger refuses it under
the subject lock account deletion takes).

**Erasure.** Account erasure deletes every row of the person in its own
step, `erase_mail_preferences`, right after `erase_skypass_wallet` and
before the services ([account-lifecycle.md](account-lifecycle.md#durable-erasure-flow)).
No suppression list is kept (ADR-0051 decision 4). The step runs whether
preferences are on or off, and a repeat finds nothing.

**Deleted teams.** Once a day (and at startup), core deletes the rows of
every Group id Keycloak no longer has. A Group list without `/UYELER`
deletes nothing. Core has no route that deletes a Group, so this is the
only path.

## API

All bodies are JSON. Errors are `application/problem+json` with a `code`:
`mail_preference_invalid` (400), `mail_preference_team_unknown` (400),
`mail_preference_person_only` (403), `mail_preference_subject_inactive`
(403), `mail_preferences_unavailable` (503, `MAIL_PREFERENCE_KEY` unset),
`mail_preference_teams_unavailable` (503 with `Retry-After`, Keycloak did
not answer the tree).

### A signed-in person

No sudo mode. A service account's token is refused (`403`).

- `GET /v1/users/me/mail-preferences` → the Account center's screen
  (`Cache-Control: no-store`):

  ```json
  {
    "master": Cell,
    "club": {"duyuru": Cell, "haber": Cell},
    "teams": [TeamRow],
    "following": [TeamRow],
    "followable": [{"groupId", "code", "displayName", "path"}]
  }
  ```

  - `Cell` is `{"explicit": true|false|null, "enabled", "origin", "from"?, "effective"}`:
    `explicit` the person's own choice on this switch (null: none),
    `enabled` the switch's value without the master switch (what the screen
    shows, greyed while the master switch is off), `origin` one of
    `explicit`, `inherited` (from the team `from`), `default`, and
    `effective` whether the mail goes.
  - `TeamRow` is `{"groupId", "code", "displayName", "path", "member", "kinds": {"duyuru": Cell, "haber": Cell}, "children": [TeamRow]}`.
  - `teams` are the trees of the top teams the person is in, every sub-team
    included.
  - `following` are the trees of the other teams the person made a choice
    on, from the topmost one; a hidden sub-team (neither it nor a team above
    it publicly listed) is left out of them.
  - `followable` are the publicly listed teams (`public_listing=true`) in
    neither list.
- `PUT /v1/users/me/mail-preferences`
  `{"items": [{"category": "team:<groupId>:duyuru", "enabled": true|false|null}]}`
  (1 to 200 items, each category once) → `200` and the new screen. `null`
  forgets the choice (back to what it inherits or its default; "unfollow",
  "reset"). `enabled` must be present. Turning a team on needs it to be in
  the person's own trees or visible (publicly listed, or below a publicly
  listed team); turning one off needs it to exist; forgetting is always
  allowed (a deleted team's row). All items are written in one transaction
  or none.

### The unsubscribe link

`GET|POST /v1/mail-preferences/unsubscribe?t=…`. No sign-in, no cookie, no
redirect: the token is the permission. `internal/mailpref`
(`Service.UnsubscribeURL`) makes it for a person and the categories a mail
was sent on; the audience route (bilet 03) gives every recipient their own,
so SkyMail never holds the key.

- `GET` shows a page naming the categories ("AIRLAB · Duyurular") and
  changes nothing, so a mail scanner that opens the link turns nothing off.
  Its three buttons `POST` the token with `kapsam`: `categories` (only
  these), `sources` (both kinds of every source the link names: "bu
  kaynaktan her şeyi kapat"), `all` (the master switch: "tümünden çık").
  It links to the Account center (`MAIL_PREFERENCE_ACCOUNT_CENTER_URL`).
  When Keycloak does not answer, or a team is gone, the page names it
  "Bir ekip"; the link works the same.
- **One-click (RFC 8058).** Every News mail carries

  ```
  List-Unsubscribe: <https://api.yildizskylab.com/v1/mail-preferences/unsubscribe?t=…>
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

**The token.** Unpadded base64url of: version (1), kind (`u`), the person's
id (16 bytes), the number of categories (1 to 32) and each category (a tag;
a team's Group id with its length), then the first 16 bytes of an
HMAC-SHA256 over all of it under a key derived from `MAIL_PREFERENCE_KEY`
for this purpose only. It carries no address and does not expire (a mail
may be opened years later). Core never logs it: the error handler logs the
path without the query, and these routes answer pages, not problem
documents (which echo the query). The edge proxy's access log must not keep
query strings of this path either.

## Settings

| Variable | |
|---|---|
| `MAIL_PREFERENCE_KEY` | 32 random bytes, unpadded base64url (OpenBao reference, set by a wizard). Unset: off, every route above `503`. Its own key, not `CONTACT_CONSENT_KEY` (which stays unset in production until the retention sweep runs). Never rotate: every mailed link depends on it |
| `PUBLIC_API_ORIGIN` | required with the key: where the links point |
| `MAIL_PREFERENCE_ACCOUNT_CENTER_URL` | the Account center page the unsubscribe page links to; unset: `https://my.yildizskylab.com` |

Startup logs `mail preferences: on|off`.
