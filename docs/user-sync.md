# User sync

Core keeps a row per person (`users`, CONTEXT "User") with the person's Sky
number. Until this sync, the row was opened only when the person's own token
first reached core (JIT), or when an admin list or another core path happened
to touch them. A person who signed up in Keycloak and never called core (sign-up
by e-mail that returns to Forms or to the Account Center's home page) had no
row, no Sky number and no Welcome mail.

The user sync closes that gap the way a directory provisioning service does
(Microsoft Entra's provisioning cycle): it reads the source of truth, Keycloak,
on a schedule and brings core in line. JIT stays as it is.

## A pass

Every `USER_SYNC_INTERVAL` (5 minutes), and soon after a wake, one core replica:

1. takes the session advisory lock `core-backend user sync` without waiting. A
   replica that finds it taken skips the pass (`passes_total{result="locked"}`).
2. reads all of Keycloak's accounts (`GET /admin/realms/{realm}/users`, brief
   form, 100 a page, 200 ms between pages) before it writes anything: a page
   that fails stops the pass with nothing written. Service accounts
   (`service-account-*`) are left out and disabled accounts are left alone.
   Keycloak lists by username; the pass takes the people in the order Keycloak
   created them (`createdTimestamp`, an account without one first), so Sky
   numbers follow sign-up order. A first request (JIT) in between still takes
   the next number at once.
3. for a person core has no row for, or whose row has no Sky number, runs the
   same upsert as JIT (`user.Service.Ensure`): it opens the row from Keycloak's
   e-mail, names and username, assigns the next Sky number and writes it to
   Keycloak's `skyNumber` attribute. On a row that exists without a Sky number
   the upsert also takes Keycloak's e-mail and username over the row's (names
   stay), as JIT does with the token's. A person with a deletion request or a
   row that is being erased or is erased is not opened again (the upsert
   refuses). At most 100 such writes per pass, 50 ms apart.
4. fills a row's empty e-mail, first name and last name from Keycloak. In this
   step a value the row has is never overwritten. (The Account Center's token carries no
   e-mail or name, so a row its first request opened is blank until this
   step.)
5. sends the Welcome mail to a person who is owed one (below). At most 25 mails
   per pass.

What a pass could not do (a Keycloak page that failed, a person whose row could
not be written, the per-pass limits) is the next pass's: nothing is lost by
stopping. A page read that fails stops the pass; a failure on one person is
counted and the pass goes on.

## The Welcome mail

The sync is the only sender of the Welcome mail. JIT, an admin's new user
(`POST /v1/users`) and, with `EVENTS_ENABLED=true`, Keycloak's events on
`core.keycloak` (a sign-up, a change of e-mail or profile, an admin's change
to a user; [integration-events.md](integration-events.md)) only wake it, so
the mail goes within seconds of a sign-up or a first request, and at worst at
the next tick. A wake never starts a pass sooner than
30 seconds after the previous one.

A person is owed the mail when all of these hold:

- the row is active and has an e-mail;
- the row's `welcome_sent_at` is empty;
- Keycloak created the account (`createdTimestamp`) on or after
  `WELCOME_MAIL_SINCE` (by default 2026-10-19 00:00 Istanbul, when sign-up by
  e-mail went live). An account Keycloak has no creation time for is older than
  that and is owed nothing.

Which path opened the row does not matter: the mark decides, so neither JIT nor
the admin list nor any other path can use up the mail.

The sync marks the row (`welcome_sent_at`) before it posts, in one conditional
`UPDATE`, so two passes cannot both send. After the post:

| Outcome (`mail.WelcomeOutcome`) | Meaning | The mark |
| --- | --- | --- |
| `sent` | SkyMail took the mail task (2xx) | kept |
| `refused` | SkyMail refused the mail itself for good (400, 413, 422: an address or body it will not take) | kept: the mail was tried and warned about once; trying every pass would repeat the refusal |
| `deferred` | SkyMail refused it for a reason about core's setup or its own state, which a fix can clear (401, 403: token, role or audience; 404: template not seeded; 409: template not publishable; 408, 429, 5xx; any other status) | taken back, the next pass sends again |
| `not_sent` | nothing left core: no token, SkyMail could not be connected to | taken back |
| `unknown` | the request went out and no answer came: SkyMail may have taken it | kept, so nobody gets the mail twice |

The post has its own 20-second bound and does not end with the pass: a
shutdown during a post lets it finish instead of leaving its outcome unknown.
A pass stopped between the mark and the post takes the mark back. A crash
between the two loses that one mail rather than sending it twice.

When the migration `20261020180000` added the column it marked every row that
had an e-mail with its `created_at`: until then the Welcome mail went out when
such a row was opened. A row without an e-mail cannot have received one and
was left unmarked, so the sync sends it once Keycloak gives the address and the
account is recent enough. A row with an e-mail that was opened after
`WELCOME_MAIL_SINCE` by a path that sent no mail (the admin list, a ticket
application, a read link) counts as welcomed; nothing in core tells it apart.
SkyMail does: the one-time operator step
`ops/wizards/core-welcome-isaret-duzelt-20261020.sh` (hub repository) takes
back the mark of such rows whose address has no `core.welcome` mail in
SkyMail since the cutoff, and the sync then sends.

## Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `USER_SYNC_ENABLED` | `true` | `false` turns the sync off: no row is opened by it and no Welcome mail goes. Any other value stops core. Without Keycloak (`KEYCLOAK_URL` unset) it is off. |
| `USER_SYNC_INTERVAL` | `5m` | Time between passes, a Go duration from `1m` to `24h`. |
| `WELCOME_MAIL_SINCE` | `2026-10-19T00:00:00+03:00` | RFC 3339. Accounts Keycloak created before it get no Welcome mail. |

The Welcome mail also needs SkyMail: `SKYMAIL_URL` (or its default) and a
welcome template key or id ([skymail-templates.md](skymail-templates.md)).
Without them the sync still opens rows and assigns Sky numbers, marks nothing
and says so at startup.

Core's Keycloak client already holds what the sync needs: `view-users` for the
list and `manage-users` for the `skyNumber` attribute (both used by the admin
user list before).

## What it tells

One log line per pass that changed something, and one per failed pass. They
carry counts and a stage, never a person:

```json
{"event":"user_sync_pass","seen":312,"created":4,"sky_number_assigned":0,"profile_filled":1,"welcome_sent":3,"welcome_refused":0,"welcome_deferred":0,"welcome_not_sent":0,"welcome_unknown":0,"deferred":0,"errors":0}
{"event":"user_sync_failed","level":"warn","stage":"keycloak","seen":0,"errors":0}
```

Every Welcome mail that does not go leaves `skymail_call_failed` with
`"kind":"welcome"` and a reason: `upstream_error` or `template_missing` with
SkyMail's status, `token_unavailable`, `unreachable`, `no_answer`
([skymail-templates.md](skymail-templates.md#what-a-refused-call-writes)).

`/v1/metrics`:

- `skylab_user_sync_enabled`, `skylab_user_sync_welcome_enabled`
- `skylab_user_sync_passes_total{result="ok|locked|failed"}`
- `skylab_user_sync_accounts_total{change="created|sky_number_assigned|profile_filled"}`
- `skylab_user_sync_welcome_total{outcome="sent|refused|deferred|not_sent|unknown"}`
- `skylab_user_sync_errors_total`, `skylab_user_sync_deferred_total`
- `skylab_user_sync_last_success_timestamp_seconds`

## Against the standard

What matches Microsoft Entra's provisioning: a scheduled pass against the
directory (Entra runs a cycle every 20 to 40 minutes; this one every 5), the
directory as the source of truth for identity fields, matching by the
immutable id (`sub`), never overwriting a value the target holds unless it is
the directory's to own, a scoping rule (enabled people, no service accounts),
and failures retried by the next cycle rather than lost.

Deviations: it is a full read every pass, not a delta (Keycloak has no delta
query and the realm is a few hundred accounts); it never disables or deletes a
row (erasure has its own path, [account-lifecycle.md](account-lifecycle.md));
and it sends the Welcome mail itself, which in Entra would be a lifecycle
workflow apart from provisioning.

## Rolling back

`USER_SYNC_ENABLED=false` and a redeploy stops it. The rows and Sky numbers it
opened stay (the same rows JIT would open). The migration's down drops
`welcome_sent_at`; the code before this change does not read it.
