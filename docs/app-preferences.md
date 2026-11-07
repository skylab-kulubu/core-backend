# App preferences

An App preference is one small per-person setting of one app, such as
whether Forms' "Formlarım" list shows details (ADR 0081 in
`skylab-kulubu/e-skylab`, CONTEXT "App preference"). Core keeps the only
copy, so the setting follows the person to another browser or phone. Core
stores, limits and deletes each value; **it never reads what is inside**.
The app gives the default; only the person's explicit choice is a row.

Code: `internal/apppref`, `internal/handlers/app_preferences.go`. Table:
`app_preferences`.

Not for: language, theme or time zone (cross-app, they get schema fields of
their own), communication preferences and consents (legal records, their
own tables), drafts and other temporary work (they stay in the browser).

## Switch and client map

`APP_PREFERENCE_CLIENTS` maps each Keycloak client (the token's `azp`) to
its app, as `app:client` pairs separated by commas. One app may have
several clients (sandbox, local); a client has one app. Unset, empty or
`none`: **off**, and every route below answers 404. A malformed value stops
core at startup.

```
APP_PREFERENCE_CLIENTS=forms:skyforms,account:account-center
```

The app `account` is Account center's: a client mapped to it also reads the
summary and resets any app. Opening a new app is a change of this value; no
migration and no code.

`azp` selects the namespace here; it never stands in for the audience. The
token must still carry `aud` `core` like every person's token core accepts
(CONTEXT "Resource server"). A client whose token has no `core` audience
(today the SkyMail console) gets it in Keycloak first.

**Production:** `forms` is turned on only after the privacy notice that
names "uygulama görünüm ayarları" is published (ADR 0081 karar 9). Until
then production leaves the variable unset; sandbox may set it.

## Routes

Every route needs a person's token. A service account's token is 403
`app_preference_person_only`. The person is always the token's `sub`: there
is no route to another person's preferences. Every answer carries
`Cache-Control: private, no-store`; the app keeps its own cache.

| Route | Who | What |
|---|---|---|
| `GET /v1/users/me/app-preferences/{app}` | the app's clients | `{"app": "forms", "items": {"forms.list": {"value": {...}, "schemaVersion": 1, "version": 3, "updatedAt": "..."}}}`; none is `"items": {}`, not 404 |
| `PUT /v1/users/me/app-preferences/{app}/{key}` | the app's clients | body `{"value": <JSON>, "schemaVersion": 1}` (`schemaVersion` optional, 1 when left out); makes or replaces the key and answers `{"app", "key", "value", "schemaVersion", "version", "updatedAt"}` with `ETag: "<version>"` |
| `DELETE /v1/users/me/app-preferences/{app}/{key}` | the app's clients | deletes the key: the app's default is back; a missing key is 204 too |
| `DELETE /v1/users/me/app-preferences/{app}` | the app's clients and Account center | deletes every key of the app ("Sıfırla") |
| `GET /v1/users/me/app-preferences` | Account center only | `{"items": [{"app": "forms", "keys": 2, "updatedAt": "..."}]}` for each configured app that holds a preference; never a value |

Refusals:

| Status | Code | When |
|---|---|---|
| 401 | | no token |
| 403 | `app_preference_person_only` | a service account's token |
| 403 | `app_preference_wrong_client` | the token's client is not the app's (Account center outside its two routes included) |
| 404 | `app_preference_unknown_app` | the app is not in the client map |
| 404 | | App preferences are off |
| 400 | `app_preference_invalid` | the body is not `{"value", "schemaVersion"}` JSON, `value` is missing or `null`, or `schemaVersion` is outside 1–1000 |
| 422 | `app_preference_invalid_key` | the key is outside `^[a-z0-9][a-z0-9._-]{0,63}$` |
| 422 | `app_preference_value_too_large` | the value is over 4 KB |
| 422 | `app_preference_quota_exceeded` | the write takes the app over 32 keys or 32 KB |
| 412 | `app_preference_conflict` | an `If-Match` that does not hold the stored version |
| 413 | `app_preference_body_too_large` | the body is over 8 KB |
| 403 | `app_preference_subject_inactive` | the person is being erased or was erased |
| 429 | `app_preference_rate_limited` | more than 60 writes (PUT and DELETE) a minute from one person, per core replica; `retryAfterSeconds` |

The app's 404 comes before the client's 403: an app outside the map is
unknown to everyone.

## Limits

- A value is at most **4096 bytes** as compact JSON (spaces the app sent do
  not count; core stores and answers the compact form).
- An app holds at most **32 keys** and **32 KB** of values per person. A
  write that replaces a key counts only its new size.
- A write's body is at most **8 KB**.
- App names: `^[a-z][a-z0-9-]{1,31}$`; keys: `^[a-z0-9][a-z0-9._-]{0,63}$`.
- The table's own CHECKs repeat the names and the 4 KB, and a loose 16 KB
  backstop on the stored `jsonb` text.

## Versions and conflicts

Each key has a `version`, 1 when made, one more on every write. The default
is last writer wins, per key: two tabs that change the same setting keep the
later one, and two screens of one app writing different keys never touch
each other. A client that cares sends `If-Match: "<version>"` (or `*`: the
key must exist) and gets 412 when the stored version differs. A deleted key
starts again at 1.

`schemaVersion` is the app's version of the value's shape. Core stores it
and never reads it: an app that changes the shape raises it, converts what
it reads, and ignores a value it cannot read.

## What an app writes

Only view state: open or closed, sort field and direction, density,
columns. Never a name, an e-mail address, form content, search text, a page
number or an open record's id. Core does not look inside the value; the
rule is the app's, and the 4 KB limit keeps free text from piling up.

The intended use (Forms, the panel):

1. On load draw from the app's `localStorage` cache keyed by the person
   (`skyforms:pref:<sub>:forms.list`), or the app's default; then one
   `GET` for the app. A stored value wins and refreshes the cache.
2. On change update the screen and the cache at once, wait a second
   (debounce, last value), then `PUT`. A failure stays silent and the next
   change tries again.
3. A value back at the default: `DELETE` the key (or write it; both are
   right).
4. A filter in the URL wins for that view; the preference only sets the
   default when the URL has none.
5. On sign-out the app drops its own cached preferences.

## Erasure and retention

Personal data, low risk; basis KVKK m.5/2-c (the service), no consent box.
Rows stay until the account goes, the person resets the app in Account
center, or the app deletes its key. No retention period of their own.

- Account erasure deletes every row of the person in the core step
  `erase_app_preferences`, right after `erase_communication_preferences`
  and before the services ([account-lifecycle.md](account-lifecycle.md)).
  The step runs whether App preferences are on or off.
- A trigger (`app_preferences_require_active_subject`) refuses a new or
  rewritten row for a person who is being erased or was erased, under the
  account deletion's own subject lock; the route answers 403
  `app_preference_subject_inactive`.
- A person deleted from the Keycloak console outside the saga loses their
  rows with their communication preferences when core hears the deletion
  event ([integration-events.md](integration-events.md)). The daily deleted
  people cleanup of the communication preferences does not cover them.
- An app taken out of the client map keeps its rows, unreachable and left
  out of the summary, until a one-time migration deletes them.
- A KVKK md. 11 request ([subject-requests.md](subject-requests.md)) is
  answered by hand; the answer includes this table's rows of the person.
  Core has no export route for them.

## Observation

On `/v1/metrics` (internal only):

- `skylab_app_preferences_requests_total{app,op,outcome}`: `op` is `get`,
  `put`, `delete`, `reset` or `summary`; `outcome` is `ok`,
  `wrong_client`, `person_only`, `unknown_app`, `invalid`, `too_large`,
  `quota`, `conflict`, `inactive`, `rate_limited` or `error`. `app` is a
  configured app, `other` for anything else, `all` for the summary.
- `skylab_app_preferences_value_bytes{app}`: a histogram of the written
  values' sizes.

Logs carry only refusals of the client (`app_preference_refused`) and
unexpected errors (`app_preference_error`), with the request id and the app.
Neither metrics nor logs ever carry a person or a value.

No server cache: core runs several replicas, reads are one primary key range
scan and writes one upsert after counting the app's rows in the same
transaction, under a per person and app advisory lock.
