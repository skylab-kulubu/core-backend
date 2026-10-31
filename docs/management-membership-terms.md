# Management membership terms

YK, DK and a Team's Leaders serve for a season (grup-yonetimi GY2, ticket
11). Core keeps a **term** for each such membership: when it ends, who is
told before, and whether it has expired. Code: `internal/membershipterm`;
command: `core-backend management-terms`; routes:
`/v1/management-memberships` (the panel's handover and extend screen,
ticket 12).

**The end is only marked** (Yusuf, 2026-10-12 22:15). Core never takes
anyone out of a Group by itself and never writes Keycloak for a term:

- 14 days and 1 day before the end, the holder and those over them are
  warned by mail;
- when the end passes, core writes `expired_at`, records it in the Team
  audit record and sends ADMIN the day's list;
- the person keeps the Group, and its rights, until someone removes them
  from the panel (the handover, or the expired term's removal below);
- once a week ADMIN hears how many expired terms are still in their Groups.

## Which memberships

A direct membership of:

| Group | Scope | `role` |
|---|---|---|
| a Privileged root that is not ADMIN (`/YK`, `/DK`, `/UYELER/YK`, `/UYELER/DK` by default; `AUTHZ_PRIVILEGED_ROOTS`), or a Group under one (`/UYELER/YK/BASKAN`) | `privileged` | `board` (YK), `audit` (DK), `privileged` (another root) |
| a Team's `LIDERLER` or `KOORDINATORLER` (`authz.TeamOf`: a Team is at least three deep under `/UYELER`) | `leader` | `team-leaders` |

No term: ADMIN's trees (`/ADMIN`, `/UYELER/ADMIN` and below), a Team's own
Group (an ordinary membership, GY7), an area's own `LIDERLER`
(`/UYELER/ARGE/LIDERLER`: an area is no Team) and every other Group. A
person in `/UYELER/YK` and in `/UYELER/YK/BASKAN` has two terms.

## The end

A new term ends with the **active season**: the end of its `end_date`'s day
in Europe/Istanbul (23:59:59; 31 May 2027 for 2026–2027). There is a
default only when exactly one season is active, it has an end date and that
day has not passed. Otherwise the term is **undated**: it never expires,
the add still goes through, and core logs
`management terms: WARN no single active season with an end ahead` and
counts it (`skylab_management_terms_route_total{event="undated"}`,
`skylab_management_terms_undated`). The daily job gives every undated term
the season's end once there is one.

So, at a season change: create the next season and make it the active one
before the handover; adds made while the old one is still active (or none
is) get its end, or none.

A term's end moves only by an extension or a handover (below), and only
within a limit: the end of the **next season** (the earliest season that
starts after the active one ends, or, when no active season has an end
ahead, the earliest not over yet, even one already begun but not set
active), or, when no next season is defined, **a year from today**
(Yusuf, 2026-10-19). The new end must be after now.

## The daily job

Each replica looks once an hour (`membershipterm.CheckInterval`); the run is
due when no scheduled run of the configured mode ended `ok` in the last 23
hours (`management_term_runs`), so runs stay at most a day apart and a
failed run is tried again within the hour. One run at a time across
replicas: a session advisory lock (`pg_try_advisory_lock`) on a connection
of its own, closed at the end; a run that finds it taken does nothing. A
run still marked `running` when the next one takes the lock is marked
`abandoned`.

A run, in order:

1. **Keycloak.** Lists the realm's Groups and reads the direct members of
   each Group with terms. A membership without an open term gets one
   (`source` `backfill`: the first rollout, or an add made in Keycloak's
   console) with the default end. An open term whose membership is gone
   (taken out in the console, the Group deleted, renamed out of scope) is
   **ended** (`ended_at`). A Group whose members cannot be read is left as
   it is this run, and the run fails. A Group list Keycloak read with a
   branch left out (a Group's children answered 404:
   `identity.ErrIncompleteGroups`) adds and ends no term this run, and the
   run fails.
2. **Dating.** With a default end, every undated open term gets it.
3. **Warnings.** An open, unexpired term within 14 days of its end and not
   warned yet gets the 14-day warning; within a day of its end and not
   warned of that, the 1-day warning. Each is sent once
   (`warned_14d_at`, `warned_1d_at`, set in the transaction that queues
   the mails); a term first seen within a day of its end gets only the
   1-day warning. A term extended between the run's read and its mark is
   not warned of the end it no longer has. Recipients below.
4. **Expiry.** An open term past its end and not marked: core opens a
   `membership.term.expired` row in the Team audit record (actor `system`,
   client `system:term-expiry`; [team-audit.md](team-audit.md)), then, in
   one transaction, writes `expired_at` and queues the list of the terms it
   marked to every member of ADMIN, then finishes the rows `ok` (`failed`
   `unchanged` for a term extended or ended in between). A row that cannot
   be opened leaves its term unmarked until the next run: no record, no
   mark. **Nothing is written to Keycloak.**
5. **Weekly summary** (`MANAGEMENT_TERMS_WEEKLY_SUMMARY`, on by default):
   when the last apply run that queued it (even a run that failed
   elsewhere), or that ended `ok` finding none to send, is a week old, the
   count of expired terms still open, by Group, goes to every member of
   ADMIN; with none, nothing goes (and the week starts again).

A step that fails does not stop the next ones; the run ends `failed` with
the first step's code (`reconcile_failed`, `warn_failed`, …) and is due
again at the next check.

### Recipients

| Term of | Told | Rule |
|---|---|---|
| YK, DK (a Privileged root's tree) | the holder; every member of ADMIN's trees and of that root's tree | grup-yonetimi 05's rule 2 |
| a Team's Leader group | the holder; the members of the Leader groups of the **Team above** (when the Team sits under a Team, not an area); every member of YK's trees | Yusuf, 2026-10-19 |
| the day's expired list, the weekly summary | every member of ADMIN's trees | GY2 |

Each person gets one mail per warning (one in ADMIN and YK gets one). The
rosters are read at the run; a roster that cannot be read leaves that
warning, or the expiry, for the next run.

## Routes

Only with `MANAGEMENT_TERMS_MODE=apply`; otherwise they are not served
(404). Bearer as everywhere.

| Route | What | Who |
|---|---|---|
| `GET /v1/management-memberships?season=&role=&status=` | the open terms, by end then path: `id`, `userId`, `firstName`, `lastName` (empty for someone erased or being erased), `groupId`, `groupPath`, `role`, `status` (`active`; `expiring`: within 14 days; `expired`: past its end), `endsAt` (null: undated), `seasonId`, `source`, `startedAt`, `grantedBy`, `warned14dAt`, `warned1dAt`, `expiredAt`. Filters: a season id, a `role` (`board`, `audit`, `privileged`, `team-leaders`), a `status` | a reader of Groups (Privileged, DK) sees every term; anyone else the terms of the Groups whose membership they may change |
| `POST /v1/management-memberships/{termId}/extend` `{"endsAt": "2027-09-30"}` | the new end (a day; its end in Istanbul). The expiry mark and both warnings go; the new end is warned of again. Answers the term | whoever may change the Group's membership, **not the holder** |
| `POST /v1/management-memberships/{termId}/handover` `{"userId": "…", "mode": "immediate" \| "at_term_end", "endsAt": "2028-05-31", "unverifiedAck": false}` | adds the successor to the Group (the member route's way: authorized, recorded as `member.add`, mailed and noticed), gives them a term (`endsAt`, or the default), and with `immediate` removes the holder the same way; with `at_term_end` the holder stays and their term is marked at its end. Answers the successor's term | whoever may change the Group's membership; nobody hands a term to themselves |
| `DELETE /v1/management-memberships/{termId}` | removes an **expired** term's holder from the Group, the member route's way (`member.remove`, the membership mail, the privilege notice), and ends the term. A term that has not expired is `409 term_not_expired`: use the member route | whoever may change the Group's membership |

"Whoever may change the Group's membership" is the member add's own
decision (`identity.Service.MembershipWriteDecision`): Privileged with
write (`groups:manage`, ADMIN, YK; DK reads only, grup-yonetimi 08: DK's
membership is changed by ADMIN and YK) for a Privileged Group; for a
Leader group, with team delegation on, a Leader who manages its Team
strictly (`ManagesStrict`: a Leader of the Team above). Every extension and
handover is recorded (`membership.term.extend`, `membership.term.handover`);
a row that cannot be opened stops the change (`503 audit_unavailable`).

Errors (problem+json): `400` a malformed id, body, day or an end not after
now; `403` (`self_extension`: one's own term, or a handover to oneself; or
the member route's refusals); `404` no such open term, or the terms off;
`409 term_not_expired`; `422 term_too_long` (past the limit above).

`GET /v1/users/me/capabilities` answers `expiredManagementMemberships`: the
caller's expired terms still open (`termId`, `groupId`, `groupPath`,
`role`, `endsAt`, `expiredAt`), for the panel's banner. `[]` unless apply.
See [authz-roles.md](authz-roles.md).

A membership changed through core's member routes starts and ends terms as
it happens: an add to a Group with terms ends any open term of the same
person and Group and opens a new one (`source` `route`, `granted_by` the
caller, the default end); a removal ends the term. That write never fails
the request: one that fails is logged and counted
(`skylab_management_terms_route_total{event="error"}`) and the daily job
finds the membership.

## Mails

One template, `club.management-term`
(`SKYMAIL_MANAGEMENT_TERM_TEMPLATE_KEY`; unset: that key, empty: off).
SkyMail's side (the template and its seed) is not in this repository: the
template has to be written and seeded in SkyMail before the mails can go.
Every key is always sent; no e-mail address is in the variables.

| Variable | Warning | Expired list | Weekly summary |
|---|---|---|---|
| `Notice` | `warning` | `expired-list` | `weekly-summary` |
| `DaysLeft` | `14` or `1` | | |
| `Self` | `yes` to the holder, `no` to the others | `no` | `no` |
| `SubjectName` | the holder's full name | | |
| `GroupName`, `GroupId` | the Group (code · `display_name_tr`; a Team's Leader group by its Team: `WEBLAB/LIDERLER · Web Geliştirme`) and its id, for the panel link | | |
| `Role` | `board`, `audit`, `privileged` or `team-leaders` | | |
| `EndsAt` | the end, `dd.MM.yyyy` in Istanbul | | |
| `Count` | | terms listed | expired terms still open |
| `Items` | | one line per term: `Name · Group · dd.MM.yyyy` | one line per Group: `Group: N` |

A Group is named as `GroupName` is, in the lists too. An expired list
whose every term was erased with its holder before it went lists nobody:
it does not go (`skipped_erased`); the marks' audit records stay.

Delivery is the membership mail's ([team-membership-mail.md](team-membership-mail.md)):
`management_term_mails` (ids, paths, counts; no address or name), one row
per recipient, sent one at a time, leased two minutes, retried after 30 s
doubling to 30 minutes, dropped after 72 hours; SkyMail's `404` (template
not seeded) retries like any failure, `400`/`422` drops the row as
`rejected`; a recipient erased, disabled or without an address is skipped.
A table of its own: an older core never reads it.

## Configuration

| Variable | Values |
|---|---|
| `MANAGEMENT_TERMS_MODE` | `off` (or unset; default): nothing is read, written or sent, the routes are not served, the capabilities answer `[]`. `dry-run`: the daily job counts what apply would write and send (its record says so) and writes nothing else. `apply`: everything above. Any other value stops core |
| `MANAGEMENT_TERMS_WEEKLY_SUMMARY` | `on` (or unset) or `off` |
| `SKYMAIL_MANAGEMENT_TERM_TEMPLATE_KEY` | the template key; empty turns the mails off (rows are still queued and dropped at their turn as `skipped_kind_off`) |

## Command

Inside the core container (it has the environment):

```
core-backend management-terms           # dry run: what apply would write and send, as numbers
core-backend management-terms --apply   # one run now; needs MANAGEMENT_TERMS_MODE=apply
```

It takes the schedule's lock, records the run (`triggered_by=cli`) and
prints counts only: whether an active season with an end ahead was found,
terms it would add (the backfill) or added, undated terms dated, terms
ended, warnings, terms marked, mails queued, the weekly summary. Exit codes:
0 done, 1 the run failed, 2 usage or configuration, 3 another run holds the
lock. A run by hand does not stand in for the schedule's day.

## Metrics (`/v1/metrics`, internal)

No person, Group or path in any of them. The gauges are read from the
tables once an hour (every replica reports the same).

| Metric | Meaning |
|---|---|
| `skylab_management_terms_mode{mode}` | 1 for the configured mode |
| `skylab_management_terms_expired_in_group` | expired terms still open: the holder is still in the Group (as the daily job last saw it) |
| `skylab_management_terms_open`, `_undated`, `_expiring` | open terms; those without an end; those within 14 days of it |
| `skylab_management_terms_active_season` | 1 when the last scheduled ok run found an active season with an end ahead |
| `skylab_management_terms_last_success_timestamp_seconds`, `skylab_management_terms_last_run_failed` | the schedule's runs |
| `skylab_management_terms_run_failures_total` | failed runs of this process |
| `skylab_management_terms_route_total{event}` | the member routes' term writes: `started`, `ended`, `undated`, `error`, `precheck_failed` |
| `skylab_management_term_mail_total{outcome}` | `sent`, `failed`, `rejected`, `expired`, `skipped_inactive`, `skipped_no_email`, `skipped_kind_off`, `skipped_erased` |
| `skylab_management_term_mail_backlog`, `_oldest_age_seconds` | the queue |

## Data

- `management_membership_terms` (migration `20261027140000`): ids only (the
  person, the Group, the season, who granted it), the Group's path, the
  scope, the end, the source, and when it started, was warned, was marked
  and ended. One open term per person and Group (a partial unique index).
- `management_term_mails`: the queue above. `management_term_runs`: one row
  per run, counts only.
- **Account erasure** (`anonymize_core`) deletes the person's terms and the
  mails that go to them or name them, and forgets them as the one who
  granted a term. The Team audit rows stay, as all of them do.
- **Retention.** The ticket bounds it by the Team audit record's three
  years and leaves the period to the ADR (grup-yonetimi §4 B). No retention
  rule deletes ended terms yet; it is added to the retention sweep once the
  ADR names the period.

## Rollout

1. Release with `MANAGEMENT_TERMS_MODE` unset (`off`). The migrations create
   the empty tables and widen the Team audit record's checks; nothing else
   changes.
2. Have an active season with its end date (2026–2027: 31 May 2027).
3. Seed `club.management-term` in SkyMail.
4. `MANAGEMENT_TERMS_MODE=dry-run` and redeploy; or, without a redeploy,
   `core-backend management-terms` in the container. Read the counts: the
   terms the backfill would write should be the YK, DK and Team Leader
   memberships the production measurement counts.
5. `MANAGEMENT_TERMS_MODE=apply` and redeploy. The first run writes the
   terms (`source` `backfill`) with the season's end; the routes open for
   the panel (ticket 12).

## Rollback

`MANAGEMENT_TERMS_MODE=off` (or `dry-run`) and redeploy: no mail, no mark,
no route; the terms stay as they are and the next apply carries on. An
older core reads none of these tables; the down migrations drop them
(`20261027140000`) and put the audit checks back without the term actions
(`20261027140100`, `NOT VALID`: rows already written stay). Keycloak needs
nothing: core never took anyone out.

## Compared with other products

What matches, and every deviation (benchmarked as the club decided,
2026-10-12):

- **Microsoft Entra PIM for Groups** gives time-bound eligible and active
  membership and ownership of a group, with an end date, and notifies
  before it ends; when the end comes, the membership ends by itself. Access
  packages' assignments expire the same way. **Matches:** a management
  membership is time-bound; the end is known when it is granted; those who
  hold and oversee it are told before it ends; extension and renewal are
  someone else's act and are recorded.
- **Google Workspace** has a membership expiration (MEMBER role only,
  Enterprise editions), tells 72 hours before and then removes the member.
- **Okta** has time-bound access requests and admin roles, revoked at the
  end.
- **Deviation 1 (the end is only marked, Yusuf 2026-10-12 22:15):** at the
  end, access stays: the term is marked expired, recorded and listed to
  ADMIN, and someone removes the person from the panel. Every product
  looked at removes the access by itself. Consequence: an expired member
  keeps the Group's rights until removed; the panel's mark, the day's list,
  the weekly summary and `skylab_management_terms_expired_in_group` keep
  that visible.
- **Deviation 2:** the term runs to a fixed date, the season's end, not for
  a duration from the moment it was granted (PIM's and Okta's durations).
- **Deviation 3:** two warnings, 14 days and 1 day before (Google: one, 72
  hours before; PIM: one before the end).
- **Deviation 4:** an ordinary Team membership has no term (GY7); only YK,
  DK and Team Leaders do. ADMIN has none (it is the break-glass and the
  overseer).
- **Deviation 5:** core keeps the terms, not the directory (Keycloak's
  Workflows are not used), because the record and the mail are core's
  (grup-yonetimi §3, GY2 sapma 4).
- **Deviation 6:** a member found in a management Group without a term (an
  add in Keycloak's console) is given one by the daily job, with the
  season's end; in PIM such an assignment would be permanent unless policy
  forbids it.
- **Deviation 7:** an extension is bounded by the next season's end, or a
  year when none is defined (Yusuf, 2026-10-19), not by a policy's maximum
  duration.
