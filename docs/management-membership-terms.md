# Management membership terms

YK, DK and a Team's Leaders serve for a season (grup-yonetimi GY2, ticket
11). Core keeps a **term** for each such membership: when it ends, who is
told before, and whether it has expired. Code: `internal/membershipterm`;
command: `core-backend management-terms`; routes:
`/v1/management-memberships` (the panel's handover and extend screen,
ticket 12).

**At the end the term is marked; 30 days later its holder leaves the
Group** (Yusuf, 2026-10-12 22:15; ADR-0078 karar 3 changed 2026-11-24):

- 14 days and 1 day before the end, the holder and those over them are
  warned by mail;
- when the end passes, core writes `expired_at`, records it in the Team
  audit record and sends ADMIN the day's list;
- the person keeps the Group, and its rights, for 30 days more: time to
  extend or hand over from the panel, or to remove them there sooner (the
  handover, or the expired term's removal below);
- once a week ADMIN hears how many expired terms are still in their Groups;
- 30 days after the end core takes the holder out of the Group itself
  ([Removal](#removal)), behind its own switch, `MANAGEMENT_TERMS_AUTO_REMOVE`
  (off by default). Off, nobody is taken out, as before.

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
   mark. Nothing is written to Keycloak.
5. **Removal** (`MANAGEMENT_TERMS_AUTO_REMOVE`; [below](#removal)): the
   holders of terms 30 days past their end leave their Groups.
6. **Weekly summary** (`MANAGEMENT_TERMS_WEEKLY_SUMMARY`, on by default):
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

## Removal

ADR-0078 karar 3 (Yusuf, 2026-11-24): an expired management membership
leaves 30 days after its end, whichever management membership it is (YK,
DK, their Groups, a Team's `LIDERLER` or `KOORDINATORLER`). The 30 days are
for an extension or a handover.

A term is **due** when it is open, marked expired, and 30 days
(`membershipterm.RemoveAfter`) have passed since both its end and its mark.
Counting from the mark too gives a term marked late (the terms turned on
after its end, a run that failed for days) its 30 days all the same; in the
ordinary case the mark comes within the hour after the end.

With `MANAGEMENT_TERMS_AUTO_REMOVE=apply`, in an apply run, the daily job
takes each due holder out, one at a time:

1. it reads the term again: one extended, handed over or ended since the
   run read it stays;
2. it removes the holder's **direct** membership of the term's Group
   through identity's system removal (`RemoveMemberAsSystem`): no person
   decides, so nothing is authorized; Keycloak's admin-console Groups are
   refused as on the member route. The Team audit record gets a
   `member.remove` row with actor `system`, the nil subject and client
   `system:term-removal` ([team-audit.md](team-audit.md)); a row that
   cannot be opened stops that removal (no record, no change);
3. the member route's removal mail goes to the holder
   ([team-membership-mail.md](team-membership-mail.md)) and, for a
   Privileged Group, the privilege notice to ADMIN and that root
   ([privilege-change-mail.md](privilege-change-mail.md), ADR-0080), both
   without an actor's name;
4. the term ends (`ended_at`).

A holder already gone from the Group only has the term ended. A removal
core refuses for good (a Group that grants Keycloak's admin console:
`keycloak_admin_group`) is held: logged, the term stays open, the run does
not fail for it; ADMIN removes it in the console. A run whose
Keycloak read failed (the reconcile step) takes nobody out: the terms may
not say what Keycloak says. A removal that fails is logged and tried again
at the next run; the run ends `failed` (`remove_failed`).

With `dry-run` the job only counts the due holders (the run's `removal`
and `removed`, the command's line). The command run by hand never takes
anyone out, even with `--apply`: it has no membership writer and counts.
Off, nothing is counted and nobody leaves.

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
| `MANAGEMENT_TERMS_AUTO_REMOVE` | `off` (or unset; default): nobody is taken out. `dry-run`: the daily job counts the holders due to leave ([Removal](#removal)). `apply`: it takes them out, when `MANAGEMENT_TERMS_MODE` is `apply` too (otherwise it only counts, and core logs a warning at start). Any other value stops core |
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
ended, warnings, terms marked, the removal's mode and the holders due to
leave (the command takes nobody out; the schedule does), mails queued, the
weekly summary. Exit codes:
0 done, 1 the run failed, 2 usage or configuration, 3 another run holds the
lock. A run by hand does not stand in for the schedule's day.

## Metrics (`/v1/metrics`, internal)

No person, Group or path in any of them. The gauges are read from the
tables once an hour (every replica reports the same).

| Metric | Meaning |
|---|---|
| `skylab_management_terms_mode{mode}` | 1 for the configured mode |
| `skylab_management_terms_expired_in_group` | expired terms still open: the holder is still in the Group (as the daily job last saw it) |
| `skylab_management_terms_removal_due` | of those, the terms due to leave ([Removal](#removal)), of Groups that still have terms (as the removal picks them): what the removal takes out at its next run, or would while it is off or a dry run |
| `skylab_management_terms_auto_remove_mode{mode}` | 1 for the removal's configured mode |
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
  per run, counts only; `removal` (`off`, `dry-run`, `apply`) and `removed`
  since migration `20261126101700`, which also lets the Team audit record's
  system basis write `member.remove`.
- **Account erasure** (`anonymize_core`) deletes the person's terms and the
  mails that go to them or name them, and forgets them as the one who
  granted a term. The Team audit rows stay, as all of them do.
- **Retention** (Yusuf, 2026-10-31): an ended term is kept **three years
  from `ended_at`**, the day its membership ended (removed through core,
  handed over, or found gone from Keycloak), and then deleted by the
  retention sweep's `management_terms_age` rule
  ([retention-sweep.md](retention-sweep.md); `RETENTION_SWEEP_MODE`). Not
  from the planned end (`ends_at`) or the expiry mark (`expired_at`): an
  expired term whose holder is still in the Group is open and stays, however
  old. The term's Team audit rows (`membership.term.*`) go by their own
  three years (`team_audit_age`). The rule does not depend on
  `MANAGEMENT_TERMS_MODE`: with the terms `off` no term ends and nothing
  here changes; the rule only deletes what an earlier apply left ended
  three years ago.

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
6. The removal: `MANAGEMENT_TERMS_AUTO_REMOVE=dry-run` and redeploy; read
   `skylab_management_terms_removal_due` and the runs' `removed` (none is
   due before 30 June 2027 for 2026–2027). Then `apply` and redeploy, at
   the latest before the first terms come due.

## Rollback

`MANAGEMENT_TERMS_AUTO_REMOVE=off` and redeploy: nobody more is taken out;
those taken out stay out (add them back from the panel; each removal's
`member.remove` row names them). `MANAGEMENT_TERMS_MODE=off` (or
`dry-run`) and redeploy: no mail, no mark, no route, no removal; the terms
stay as they are and the next apply carries on. An
older core reads none of these tables; the down migrations drop them
(`20261027140000`) and put the audit checks back without the term actions
(`20261027140100`, `NOT VALID`: rows already written stay); the removal's
down (`20261126101700`) drops the runs' two columns and leaves the audit
check wide, so its rows stay.

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
- **Deviation 1 (30 days' grace, Yusuf 2026-10-12 22:15 and
  2026-11-24):** at the end, access stays for 30 days: the term is marked
  expired, recorded and listed to ADMIN; then core removes the membership
  by itself. Every product looked at removes the access at the end.
  Consequence: an expired member keeps the Group's rights for 30 days (or
  until removed sooner); the panel's mark, the day's list, the weekly
  summary and `skylab_management_terms_expired_in_group` keep that
  visible. **Matches:** the removal itself is automatic and recorded, as
  in PIM, Google and Okta; behind a switch off by default with a dry run
  first (a safe rollout, as Entra's access reviews' "auto apply" is a
  setting the owner turns on).
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
