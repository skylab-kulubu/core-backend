# News audience

Who a News send (duyuru, haber) goes to, decided by core for SkyMail
(ADR 0067 in `skylab-kulubu/e-skylab`; sky_lab_genel
`.scratch/duyuru-haber`, bilet 03, SPEC §5.8). SkyMail asks with its
service account, core answers the recipients with their Primary e-mail,
the categories they entered through and each one's own unsubscribe link,
so SkyMail never holds the link key and never resolves Keycloak Groups for
this itself.

Code: `internal/commpref` (`Audience`, `audience.go`), the route
`internal/handlers/audience.go`, the Keycloak reads
`internal/identity/audience_members.go`. It reads the preferences of
[`communication-preferences.md`](communication-preferences.md) and adds no
table.

## The route

`POST /v1/audiences/resolve[?countOnly=true][&after=<userId>][&limit=1..500]`

Only a **service account** token holding core's client role
`audience:read` (`resource_access.core.roles`; SkyMail's, bilet 04) is
served. A person's token is refused `403 audience_service_only` whatever it
carries; a service account without the role `403 audience_role_missing`.
`Cache-Control: no-store` on every answer.

Body (JSON, unknown fields refused):

```json
{
  "type": "members" | "teams",
  "teams": ["/UYELER/ARGE/WEBLAB", "<group id>"],
  "newsKind": "duyuru" | "haber",
  "visibility": "herkes" | "uyeler" | "ekipler",
  "visibilityTeams": ["/UYELER/ARGE/WEBLAB/FRONT"]
}
```

- `teams`: only with `type: "teams"`, 1 to 32, each a team's Group path or
  id. A team is a Group `teammail.TeamOf` calls a team's own
  (`/UYELER/<area>/<team>` or a sub-team below); an area, a leader
  subgroup (`LIDERLER`, `KOORDINATORLER`), a Privileged Group or an unknown
  one is `400 audience_team_unknown`.
- `visibility`: the News record's. In Faz 1 an absent one is `herkes` (the
  field's absence means public, ADR 0067 karar 3). With
  `NEWS_AUDIENCE_PRIVATE_VISIBILITY=on` it is **required** (`400
  audience_invalid` without it): once private records exist, a send that
  does not say its record's visibility could open a private record to the
  followers.
- `visibilityTeams`: only with `ekipler`, 1 to 32 teams, as `teams`.

### Faz 1: only public records

ADR 0067 karar 3 opens private records in a second step. Until then
(`NEWS_AUDIENCE_PRIVATE_VISIBILITY` unset or `off`, the default) only
`herkes` is accepted; `uyeler` and `ekipler` answer `400
audience_visibility_not_enabled`. The rules below for them are built and
tested already; the second step is setting the variable to `on`.

**It is turned on only after** inscribed's per-record visibility (Fatih's
F2) is in production **and** the leak tests (duyuru-haber bilet 21) are
green: before that a private record does not exist on the web, and a send
to `uyeler` or `ekipler` would promise a privacy the record does not have.
While it is on, startup logs a `WARNING news audience: …` line naming
both conditions. Any value other than `on`/`off` stops startup with an
error that names the variable.

### Answer

```json
{
  "items": [
    {
      "userId": "…",
      "email": "ad.soyad@example.com",
      "name": "Ad Soyad",
      "categories": ["team:<groupId>:duyuru"],
      "via": "member" | "follower",
      "reason": "WEBLAB · Web Geliştirme ekibinde olduğun için",
      "categoryLabel": "WEBLAB · Web Geliştirme · Duyurular",
      "unsubscribeUrl": "https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=…"
    }
  ],
  "next": "<userId>" | null,
  "counts": {"members": 0, "followers": 0, "total": 0},
  "skipped": {"notMember": 0, "notVisible": 0, "disabled": 0, "noEmail": 0, "unverified": 0, "optedOut": 0, "duplicateEmail": 0}
}
```

- `email` is the Primary e-mail (Keycloak `email`); core never answers a
  second address.
- `name` is core's profile name, Keycloak's when core has no row.
- `categories` are the categories the person entered through that are on
  for them, membership ones first. `unsubscribeUrl` turns exactly these
  off (one-click, RFC 8058; [communication-preferences.md](communication-preferences.md#the-unsubscribe-link)).
- `via`: `member` when at least one category comes through membership,
  else `follower`. `reason` is the first category's: "SKY LAB üyesi
  olduğun için" (members), "<ekip> ekibinde olduğun için" (the team the
  person is in: the named one or one above it), "<ekip> ekibini takip
  ettiğin için" (the team whose choice they follow: the named one or one
  above it).
- A team is named as the membership mail names a Group (`teammail`'s
  GroupName, #238): "KOD · display_name_tr", the code alone when it has no
  Turkish name (or the same one): "WEBLAB · Web Geliştirme", "AIRLAB".
- `categoryLabel`: "Kulüp geneli · Duyurular", or the categories' teams
  named so, "WEBLAB · Web Geliştirme, AIRLAB · Haberler".
- `counts.total` = `members` + `followers` = the number of recipients over
  all pages.
- `?countOnly=true` answers `counts` and `skipped` only (no `items`, no
  `next`, no address): the live count of SkyMail's form. Its numbers are the
  list's.

### Pages

Recipients are in user id order. A page holds at most `limit` (default and
maximum 500); `next` is the last user id of the page when more follow, and
`?after=<that id>` asks the next page (exclusive). Every page computes the
whole audience again on the snapshot of the moment (below), so `counts` and
`skipped` are the whole audience's on every page. Within one snapshot the
pages neither repeat nor miss anyone; a person who joins or leaves between
two pages that straddle a new snapshot may be missed or left in, never
sent twice. That holds for a **person**, not for an **address**: if two
accounts share an address, the lower user id keeps it on one page, and a
later page computed after that person turned the category off (or on a
new snapshot) may answer the other account with the same address. SkyMail
checks the address too before sending a News send (bilet 06).

### Errors

`application/problem+json` with a `code`:

| Status | `code` | When |
|---|---|---|
| 400 | `audience_invalid` | a body that does not read, an unknown field or value, a list too long or present where it may not be, `limit` outside 1–500, `after` no UUID, `countOnly` not `true`/`false` |
| 400 | `audience_team_unknown` | a name in `teams` or `visibilityTeams` that is no team |
| 400 | `audience_visibility_not_enabled` | `uyeler` or `ekipler` in Faz 1 |
| 403 | `audience_service_only` | a person's token |
| 403 | `audience_role_missing` | a service account without `audience:read` |
| 503 | `audience_unavailable` | no link key set (`COMMUNICATION_PREFERENCE_KEY` or a newer version): no link can be signed |
| 503 | `audience_directory_unavailable` (`Retry-After: 30`) | Keycloak did not answer, or its Group list has no `/UYELER` |

## Who is in it

### Candidates

- **`members`**: everyone directly in a Group of the `/UYELER` tree
  (`/UYELER` itself, an area, a team, a leader subgroup, a Privileged root
  under `/UYELER`). Category `club:<kind>`.
- **`teams`**, for each named team T (category `team:<T>:<kind>`):
  - **members**: everyone in T or a team below it (leaders included). A
    Privileged root inside T (`AUTHZ_PRIVILEGED_ROOTS`) holds no team, so
    its members are not T's;
  - everyone in a team above T: the default reaches sub-teams. This is
    Yusuf's Y1 decision (2026-10-12: a sub-team's announcement goes to the
    team above's subscribers; Karar D5, "üst ekibe abonelik alt ekipleri
    kapsar") and the inheritance of the preferences (#232,
    [communication-preferences.md](communication-preferences.md#values)):
    a WEBLAB member gets the News of WEBLAB's sub-team BACK unless they
    turn BACK off, so the Account center's screen and the mail agree. They
    count as members (`via: member`, reason names the team they are in);
  - **followers**: the **Members** who turned the kind on, on T or a team
    above it (Karar D5, D7; an off row follows nothing). Faz 1 mails
    Members only (SPEC §10.2: "Faz 1 yalnız `/UYELER` ağacına gönderir"):
    a follower in no `/UYELER` Group (a row left from before core refused
    it, or a former Member's) is left out and counted `notMember`, and is
    not read from Keycloak. A subscriber who is no Member is Faz 2's, on a
    Contact consent (SPEC §5.8, K8), not a preference; core refuses such
    a follow ([communication-preferences.md](communication-preferences.md#a-signed-in-person)).
  - A **hidden** T (no `public_listing=true` on it or above it, O2) reaches
    its own members only: nobody above it and no follower is a candidate,
    so none is counted either.

### Visibility

| `visibility` | Who may get it |
|---|---|
| `herkes` | every candidate |
| `uyeler` | Members only (every candidate already is one in Faz 1) |
| `ekipler` | members (as above) who are in a `visibilityTeams` team or a team below one; every follower is `notVisible` (Karar D7: a team-only record never goes to a follower), and so is everyone else outside |

### Filters

Each candidate is counted once, under the first of these that holds:

1. a client's **service account** (Keycloak `serviceAccountClientId`, or a
   username starting `service-account-`): no person, left out and **not
   counted**;
2. `notMember`: a follower in no `/UYELER` Group (above);
3. `notVisible`: above;
4. `disabled`: disabled in Keycloak, or being erased or erased in core (an account deletion request, a non-active
   account or the deletion marker; the placeholder subject);
5. `noEmail`: no Primary e-mail;
6. `unverified`: Keycloak `emailVerified` false;
7. `optedOut`: every category they entered through is off for them
   (the channel's master switch, the category, or what it inherits; the
   rules of [communication-preferences.md](communication-preferences.md#values)
   on channel `email`). Entering through several teams, one on is enough;
8. `duplicateEmail`: an earlier recipient (lower user id) has the same
   address, lower-cased.

Duplicates by person go first: a person who enters through several teams
and as a follower is one candidate, with every category that is on.

A Keycloak member record core cannot read (no id, not a UUID) is left out
and counted nowhere; Keycloak does not answer such records for real users.

## Reads and cost

- **Keycloak** (core's service account, the Group reads core already has;
  [keycloak-admin-permissions.md](keycloak-admin-permissions.md#news-audience)):
  the Group tree (`GET /groups`, `GET /groups/{id}/children`), then the
  direct members of every `/UYELER` Group in full representation (`GET
  /groups/{id}/members?briefRepresentation=false`, 200 a page, four Groups
  at a time). Nobody is read one by one. One such snapshot serves every
  request for 30 s (concurrent ones share one read, which runs on its own
  context with a 30 s limit, so one caller giving up does not fail the
  others): the live counts typed against a form and the pages of one send
  cost one read. An expired snapshot (every Member's address and name) is
  dropped when the next request finds it expired; between requests it
  stays in the process's memory, at most until then.
- **Core's database**: the preference rows of every candidate in one query,
  the people who turned the kind on on the teams' chains in one, and
  core's account state and names in one.

## Personal data

The answer carries addresses and names, to SkyMail's service account only.
Core logs one JSON line per request with counts and no person:
`{"event":"news_audience","client":"skymail","type":"teams","teams":1,"kind":"duyuru","visibility":"","count_only":false,"page":12,"total":12,"members":10,"followers":2,"not_member":0,"not_visible":0,"disabled":1,"no_email":0,"unverified":0,"opted_out":3,"duplicate_email":0}`.
Keycloak errors name a status, never a person or a Group. The unsubscribe
links are never logged (see their section).

## Authorization note

The route checks a service account and the role, not which client it is:
whoever Keycloak maps `audience:read` to may call it, as with
`consent:audience:read`. The control is the role mapping (bilet 04: only
SkyMail's service account). A check on the client id would add defence in
depth; it is not done.

## Settings

| Variable | |
|---|---|
| `COMMUNICATION_PREFERENCE_KEY` (or its key ring) | required: without a key version the route answers `503 audience_unavailable` ([communication-preferences.md](communication-preferences.md#settings)) |
| `NEWS_AUDIENCE_PRIVATE_VISIBILITY` | `off` (default): only `herkes`, an absent `visibility` is `herkes`. `on`: `uyeler` and `ekipler` too, and `visibility` is required (ADR 0067 karar 3, second step) |

Startup logs `news audience: on|off (private visibility true|false)`, and
with `on` the warning above.

## Compared with the industry

What matches:

- **Service-to-service authorization**: an OAuth 2.0 client-credentials
  token with an application role, as a daemon app holds an application
  permission (app role) in Microsoft Entra ID; a user's token never
  reaches the route.
- **Recipient filtering before the send**: the sender asks for the list of
  people who may get this message, already filtered by subscription, as
  Ghost sends a newsletter only to members subscribed to that newsletter
  and Mailchimp a campaign only to subscribed contacts of the segment.
- **Per-list unsubscribe in every mail** with a signed link per recipient
  and RFC 8058 one-click (Gmail's and Yahoo's bulk sender requirements;
  Ghost's per-newsletter unsubscribe).
- **Nested groups expanded**: a team's mail reaches its sub-teams'
  members, as a mail to an Exchange distribution group or a Google Group
  reaches the members of nested groups.
- **Dead or unwanted addresses are not mailed**, as Mailchimp sends no
  campaign to unsubscribed or cleaned contacts, and **an address gets one
  copy** (a Mailchimp audience holds each address once).
- **A live count** before the send (Mailchimp's and Beehiiv's recipient
  count on the review screen).
- **Cursor pages** keyed by the last id, as Stripe's `starting_after`.
- **Data minimization**: addresses and names in the answer only, counts in
  the logs (KVKK md. 4, GDPR art. 5(1)(c)).

Deviations (all):

1. **The audience service signs the unsubscribe links**, not the sender:
   in Mailchimp and Ghost the mail system itself holds the subscription
   and the link. Here the preference lives in core (Karar D3) and SkyMail
   must not hold its key.
2. **Default inheritance down the team tree** (Y1, D5, approved for the
   audience 2026-10-12): a team's member
   gets its sub-teams' News by default. None of the products above has a
   subscription tree; it follows the club's team tree.
3. **`emailVerified` is required.** Ghost and Mailchimp send to whoever is
   on the list (Mailchimp's double opt-in is the list's choice). Here a
   person's Keycloak address may be unconfirmed, and mailing it would mail
   whoever typed it.
4. **The cursor is a user id, not an opaque token**, and every page is
   computed again (on a 30 s snapshot) instead of from a stored result:
   Microsoft Graph's `@odata.nextLink` is an opaque link. A send
   reads its pages back to back, so the snapshot covers them; a stored
   result would be a second copy of addresses.
5. **Service accounts in a Group are left out and not counted**:
   Exchange and Google Groups mail any member address. A Keycloak service
   account has no person behind it.
6. **A second account with the same address is skipped and counted**
   (`duplicateEmail`) instead of silently merged, so the numbers add up.
7. **Private visibility is refused in Faz 1** although its rules are
   built: ADR 0067 opens private records only after inscribed's
   per-record visibility and the leak tests.
8. **Following is for Members only in Faz 1**: Ghost and Substack let
   anyone subscribe with an address. Here a non-Member subscriber needs a
   Contact consent (SPEC §5.8, Faz 2), so a follow is a Member's
   preference only.

## Recipients

The second question SkyMail asks (ADR 0072 karar 3 in
`skylab-kulubu/e-skylab`; sky_lab_genel `.scratch/abonelik-cikisi`, bilet
01): not "who gets this News send" but, for recipients **a sender chose**
(a list, a Keycloak Group, a form's respondents, an approved send), which
of them are a person core knows, whether the mail may go to each in one
category, and each one's unsubscribe link. SkyMail asks it at its one
check point, when it queues a duyuru-stream send and again in every
dispatch round right before sending. The routes above, the one-click
route and `tracking:resolve` do not change.

Code: `internal/commpref/recipients.go` (`Recipients`), the route
`internal/handlers/recipients_resolve.go`, the Keycloak reads
`internal/identity/address_directory.go`. No table, no migration: it only
reads.

### The route

`POST /internal/v1/audiences/recipients:resolve`

The same caller as the audience route: a **service account** token with
core's client role `audience:read` (SkyMail's, already mapped by
e-skylab-keycloak's reconcile; nothing new in Keycloak). A person's token
is `403 recipients_resolve_service_only` whatever it carries; a service
account without the role `403 recipients_resolve_role_missing`. It is an
`/internal` route: a request that came through the public edge
(`X-Forwarded-*`, `Forwarded`, `X-Real-Ip`) is a bare `404` before its
token is read. `Cache-Control: no-store` on every answer.

```json
{"channel": "email", "category": "team:<groupId>:duyuru",
 "recipients": [{"email": "a@ornek.com"}, {"sub": "7d3c1f6e-…"}]}
```

- `channel`: `email` only.
- `category`: `club:duyuru`, `club:haber`, `team:<groupId>:duyuru` or
  `team:<groupId>:haber`. `all` and `tracking` are no category here. A
  Group `teammail.TeamOf` calls no team's own (an area, a leader subgroup,
  a Privileged root, an unknown id) is `400
  recipients_resolve_category_unknown`. A hidden team is a team: its own
  mail is its sender's to send, and the answer does not tell a hidden team
  from a listed one.
- `recipients`: 1 to 1000, each exactly one of `email` (compared trimmed
  and lower case; it must hold an `@`, at most 320 characters) or `sub`
  (a lower-case hyphenated UUID). The same recipient twice (addresses
  compared trimmed and lower case) is `400 recipients_resolve_invalid`.
  Unknown fields are refused.

Answer, one result per recipient in the order asked:

```json
{"results": [
  {"matched": true, "sub": "…", "allowed": true, "reason": null, "unsubscribeUrl": "https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=…"},
  {"matched": false},
  {"matched": true, "sub": "…", "allowed": false, "reason": "opted_out"},
  {"matched": true, "allowed": false, "reason": "ambiguous"}
]}
```

- `matched: false` (alone): no account holds the address as a verified
  address (below), or no account (that is no service account) has the
  sub. SkyMail treats it as an address without an account: its own
  record and its own link (ADR 0072 karar 4).
- `unsubscribeUrl` only when `allowed`: the person's link for exactly
  this category (`Service.UnsubscribeURL(sub, email, [category])`, the
  shape the audience route answers; one-click, RFC 8058,
  [communication-preferences.md](communication-preferences.md#the-unsubscribe-link)).
- `sub` is left out for `ambiguous`.

### Errors

| Status | `code` | When |
|---|---|---|
| 400 | `recipients_resolve_invalid` | a body that does not read, an unknown field, a channel other than `email`, a category that is no club or team category, no recipient or more than 1000, an item with both or neither field, a malformed address or sub, the same recipient twice |
| 400 | `recipients_resolve_category_unknown` | `team:<groupId>:…` whose Group is no team's own |
| 403 | `recipients_resolve_service_only` | a person's token |
| 403 | `recipients_resolve_role_missing` | a service account without `audience:read` |
| 404 | (bare) | through the public edge |
| 503 | `communication_preferences_unavailable` | no link key set (`COMMUNICATION_PREFERENCE_KEY` or a newer version): no link can be signed |
| 503 | `recipients_resolve_directory_unavailable` (`Retry-After: 30`) | the verified-address directory was never read, or is older than 15 minutes and Keycloak does not answer; or Keycloak's Group list does not answer a team category. SkyMail then writes no send (fail closed, ADR 0072) |

### Matching: verified addresses (ADR-0044)

An address is a person's when it is one of the verified identifiers the
Keycloak login form (`sky-username-password-form`, e-skylab-keycloak
`LoginIdentifiers`) takes, compared trimmed and lower case. This is
stricter than the login form, which also takes the username and the
Primary e-mail unverified: mapping an unverified address to an account
would hand its link, which turns the account holder's category off, to
whoever owns that address. The addresses matched:

- the Primary e-mail (`email`) while `emailVerified` is true;
- the School e-mail (`schoolEmail`) only while the account is linked to
  the YTÜ Microsoft identity provider (`KEYCLOAK_YTU_IDP_ALIAS`, `OBS`):
  the Account center's `verifiedYtu`. The link comes from Keycloak's
  federated identities (the `idpAlias` read below), not from core's
  `users.ytu_linked`, which is empty for a person who never signed in to
  a core application;
- the Personal e-mail (`personalEmail`) only while its proof stamp
  `personalEmailVerifiedAt` reads as a moment (sky-account writes it when
  the code is confirmed).

The attribute names are `erasureAddressAttributes`'. Each attribute's
first value is read, as the login form reads it. A client's service
account (`serviceAccountClientId`, or a username starting
`service-account-`) is never matched. An address two or more accounts hold
is `ambiguous`: no single preference stands for it, so it is never
allowed.

### Decision

Dynamics' "nonrestrictive" model: the sender chose the list, so only the
person's own "off" holds the mail back; a default never does (ADR 0072
karar 3). The first that holds is the answer:

| # | `reason` | When |
|---|---|---|
| 1 | `inactive` | disabled in Keycloak, or being erased or erased in core (the account state the audience route's `disabled` reads; the placeholder subject) |
| 2 | `opted_out` | the channel's master switch (`all`) is off |
| 3 | `opted_out` | the explicit choice that governs the category is off: for `club:<kind>` the club row; for `team:<id>:<kind>` the most specific explicit choice of the kind from the team up through the teams above it ([inheritance](communication-preferences.md#values)) |
| 4 | — | `allowed: true`, with the link |

Every default counts as on (`Tree.ResolveChosen`, the same value
computation as the screen and the audience with its defaults turned on,
no second copy of the inheritance): club-wide mail for someone who is no
Member (a guest, a Place user on an operator's list; Yusuf, 2026-10-21),
a team nobody follows, and a hidden team the person is not in. O2 ("a
hidden team reaches its own members only") decides whom a News send
reaches by default; here it is a default like the others. An explicit
"off" on a team above a hidden team holds for it too, as for any other
sub-team. Membership plays no part, so no Group of the person is read.

What differs from the audience route:

| | `POST /v1/audiences/resolve` | `recipients:resolve` |
|---|---|---|
| Who picks the people | core, from the team tree and the preferences | the sender |
| A default off (not a Member, a team not followed, a hidden team) | left out | allowed |
| Address | the Primary e-mail only | any verified address (Primary, YTÜ-linked School, proven Personal) |
| Unknown address | — | `matched: false`, SkyMail's own record applies |
| Link | the categories the person entered through | the one category asked |

### The verified-address directory

Most recipients have no account, and asking Keycloak per address (an
exact e-mail search and two attribute searches) would cost tens of seconds
for 1000 of them. core's user shadow is incomplete (only people who signed
in to a core application). So core reads the verified addresses of the
whole realm in bulk and keeps them in memory:

- **Reads.** The accounts linked to the YTÜ identity provider (`GET
  /users?idpAlias=…&briefRepresentation=true`, their ids), then every
  account (`GET /users?briefRepresentation=false`; `enabled`, `email`,
  `emailVerified`, the three attributes, `serviceAccountClientId`), 500 a
  page, one page at a time, one read at a time.
- **A set per address.** Each address holds the set of accounts it
  belongs to: offset paging may answer an account twice when the realm
  changes during the read, and a list would then call its address
  ambiguous. The other way round, an account deleted during the read
  moves the rows after it back by one, so the account at the next page
  boundary may be missed in that read and count as no account until the
  next one (ADR 0072 deviation 1; unlikely: production's 186 accounts are
  read in about 0.3 s).
- **Only a complete read replaces the directory**, at once. A read that
  fails halfway is dropped: a half directory would call people
  account-less and break the `ambiguous` decisions. The old one stays as
  it was.
- **On demand, no timer.** A question that finds the directory 5 minutes
  old answers from it and starts a new read; concurrent questions share
  that read and are answered from the old directory meanwhile. Nothing is
  read while no question comes. With no directory, or one 15 minutes old,
  the question waits for the read and is refused `503` if it fails; after
  a failed read no new one starts for 30 s (Keycloak gets one attempt in
  that time, not one per question). A read runs on its own context (3
  minutes at most): a caller giving up does not stop it.
- **What is fresh.** The person's preferences and core's account state
  are read on every question. `enabled` and the address → person mapping
  come from the directory: at most 5 minutes old (15 while Keycloak does
  not answer). Known lag (ADR 0072 deviation 1): an account opened, or an
  address verified, in that window counts as no account; a person who had
  turned the category off in core and whose newly verified address is on
  an operator's list gets that mail, with SkyMail's own link, which does
  not carry the core opt-out. A Keycloak disable lags as long.
- **Personal data.** The directory holds addresses and subs in core's
  memory only; it is never written or logged. One JSON line per question,
  counts only:
  `{"event":"recipients_resolve","client":"skymail","scope":"team","kind":"duyuru","asked":1000,"matched":12,"allowed":10,"inactive":1,"opted_out":1,"ambiguous":0}`.
  No address, sub or link is logged; Keycloak errors name a status only.
- **One line per directory read**, counts only, from the read itself (so
  a refresh that fails in the background is seen too):
  `{"event":"recipients_directory_read","ok":true,"ms":310,"accounts":186,"linked":120,"school_email":130,"primary_unverified":4}`:
  the accounts read (service accounts left out), those linked to the YTÜ
  identity provider, those holding a School e-mail, and those whose
  Primary e-mail is unverified and so not matched. A failed read logs
  `{"event":"recipients_directory_read","level":"warn","ok":false,"ms":…,"error":"identity: address directory read failed with status 502"}`.
  Keycloak answers an unknown `idpAlias` with an empty list, so a wrong
  `KEYCLOAK_YTU_IDP_ALIAS` would silently match no School e-mail: when no
  account is linked but some hold a School e-mail, a second line
  `{"event":"recipients_directory_alias_suspect","level":"warn","alias":"OBS","linked":0,"school_email":130,"hint":"… KEYCLOAK_YTU_IDP_ALIAS may be wrong"}`
  says so. Opening the route expects `linked > 0` on the first read.

### Measurements

Measured 2026-10-21 on a local Keycloak 26.4.0 (`start-dev`, 2 CPUs,
1.5 GiB) with a realm of 2000 accounts (1200 linked to the identity
provider, 3400 verified addresses), ten times production's 186 accounts
(2026-10-12):

| | |
|---|---|
| holders read, 100 a page | 21 pages, 0.36 s (1.6 s the first time, cold Keycloak) |
| holders read, 200 a page | 11 pages, 0.29–0.30 s |
| holders read, **500 a page** (chosen) | 5 pages, 0.22–0.24 s |
| holders read, 1000 a page | 3 pages, 0.17–0.20 s |
| `idpAlias` read, 500 a page | 3 pages, 0.13 s |
| whole directory (both reads) | 0.36 s, 448 KiB of heap |
| 1000 addresses without an account, cold (the read included) | 0.31 s |
| 1000 addresses without an account, warm | 0.08–0.25 ms, no Keycloak call |
| 1000 mixed (400 matched), warm, team category | 0.7 ms (preferences in memory; production adds two indexed database reads) |

500 a page: four times fewer requests than 100 for a few tens of
milliseconds more than 1000, with smaller answers for Keycloak to build.
The warm target (< 200 ms for 1000 recipients) holds with room. The
directory grows linearly: some 220 bytes an account. The test
`TestRecipientsThousandUnknownAddressesReadNoAddress` checks that the reads
are the directory's pages, whatever the number of addresses.

### Settings

| Variable | |
|---|---|
| `COMMUNICATION_PREFERENCE_KEY` (or its key ring) | required: without a key version `503 communication_preferences_unavailable` |
| `KEYCLOAK_YTU_IDP_ALIAS` | the YTÜ Microsoft identity provider's alias (e-skylab-keycloak's `ytuIdpAlias`); unset: `OBS`. A value that is no alias stops startup with an error that names the variable. A wrong alias reads no link, and no School e-mail matches |

Startup logs `recipient question: on|off (YTÜ identity provider OBS)`.

### Compared with the industry

What matches:

- **Consent checked right before sending**, per recipient and per topic:
  Dynamics 365 Customer Insights – Journeys enforces contact point
  consent and the purpose's topic at send time; HubSpot subscription
  types, SES contact list topics.
- **Nonrestrictive by default**: Dynamics' Commercial purpose sends until
  the contact opts out; only an explicit opt-out holds the mail back.
- **Per-topic unsubscribe link** in every mail (Google's and Yahoo's bulk
  sender rules; RFC 8058 one-click).
- **Bulk directory instead of per-recipient lookups**, refreshed on a
  short interval: the way a send pipeline resolves a segment once, not per
  message.
- **Service-to-service authorization** with an application role on a
  client-credentials token; a user's token never reaches the route.

Deviations (all):

1. **The preference is a person's, not an address's** (ADR 0072
   deviation 1): Dynamics and HubSpot key consent by address. Here an
   address must first be mapped to a person, and an address two accounts
   hold gets no mail.
2. **The mapping may be 5 minutes old** (15 while Keycloak does not
   answer): the price of not asking Keycloak per address. The preference
   and the erasure state are never stale.
3. **The School e-mail counts only while linked to YTÜ** and the Personal
   e-mail only while proven, and the Primary e-mail only while verified:
   the login form's verified identifiers (the form also takes the
   username and an unverified Primary e-mail), stricter than an
   address-keyed product, which takes any address on its list.
