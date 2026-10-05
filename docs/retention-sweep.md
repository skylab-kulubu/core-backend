# Retention sweep (periodic destruction)

Core's periodic destruction run (ADR-0062, KVKK deletion regulation art. 7 and
11). Once a day it empties the personal fields of the rows its rules find past
their retention period, or deletes a row whose removal is the fact itself,
and counts the rows core's hourly cleanups should already have removed. Every
run writes a record without personal data that is kept at least three years.
Code: `internal/retention`; command: `core-backend retention-sweep`.

Other services (SkyMail, Forms, Place, Guessr) destroy their own data with
their own runs (ADR-0062: each service its own data); core never writes
another service's database.

## Modes

`RETENTION_SWEEP_MODE`:

| Value | What core does |
|---|---|
| `off` (or unset; default) | nothing: no query, no metric. The record tables exist (the migration creates them) and stay empty |
| `dry-run` | the daily run counts what apply would change, per rule, and changes nothing |
| `apply` | the daily run changes the rows |

Any other value stops core at startup, naming the variable. The dry run and
apply are generated from the same rule definition (one `WHERE` clause per
rule), so they cannot drift: apply's batch selects exactly the rows the dry
run counted (`TestDryRunAndApplyShareOneWhereClause`,
`TestDryRunCountsWhatApplyChangesAndApplyIsIdempotent`).

**Apply also changes two hourly cleanups**, because only apply has something
that empties an address after a year (ADR-0062 consequences):

| Cleanup | `off`, `dry-run` (as before) | `apply` |
|---|---|---|
| short-link clicks (`url_hits`) | the hourly cleanup deletes rows older than 90 days | rows are kept; `url_hits_scrub` empties their personal fields after a year |
| private Media access log (`media_read_links`, `_opens`) | the hourly cleanup deletes links and opens older than a year | kept three years; `read_link_ip` empties an open's address after a year |

Switching apply off brings the old windows back at the next start (the
hourly cleanup then deletes the kept rows older than 90 days / a year). The
click list and a form's channel statistics keep looking 90 days back
(`shorturl.HitListWindow`) in every mode.

**Side effect on the click counter:** in apply, `urls.click_count` (the
"clicks" figure of a short link in the admin panel) no longer drops when
clicks turn 90 days old. Until apply it is the clicks of the last 90 days;
from apply on it only grows, counting every click since then.

## Rules (rule set v2)

"Event end" is the Event's `end_date`, else its last day's `end_date`, else
its `start_date` (ADR-0062). A row whose Event has none of them is never
changed; it is counted as `anchorless`. Archived Events count like any other:
archiving does not stop the clock (ADR-0042). A guest is a Ticket without an
owner, as in account erasure.

| Rule | Table, columns | Anchor | Period | Action | Kept / left out |
|---|---|---|---|---|---|
| `guest_phone` | `tickets.guest_phone_number` | the Ticket's Event end | 90 days | emptied | consent or not (ADR-0062) |
| `guest_identity` | `tickets.guest_first_name`, `guest_last_name`, `guest_email`, `guest_phone_number`; `certificates.recipient_email` of the Ticket's certificates without an owner, in the same statement | the person's **latest** Event end: every guest Ticket of the same address (trimmed, lower-cased), and every Ticket of the account whose `email` or `school_email` is that address (a guest who later became a member; v2); a Ticket without an address, its own Event | 2 years | emptied; the Ticket, its check-ins and its certificate (name, serial, PDF) stay | a Ticket with a queued or running certificate job; an address with an active (confirmed, open) `event_invitations` consent given for the address |
| `door_staff` | `event_door_staff` | the Event end | 90 days | row deleted (a relationship row) | |
| `url_hits_scrub` | `url_hits.ip`, `user_agent`, `user_id`, `utm_campaign`, `utm_term`, `utm_content`, `referer` | `at` | 1 year | emptied; `referer` keeps its origin (`scheme://host`, lower-case, no user information, port, path, query or fragment); the row (time, link, alias, `utm_source`, `utm_medium`: the channel the statistics count) stays. The free UTM fields are whatever the link's author or the visitor typed into the address (an e-mail, a student number), so they go with the address (v2; account erasure already clears them) | only does anything in apply (above) |
| `read_link_ip` | `media_read_link_opens.client_ip` | `opened_at` | 1 year | emptied; the open stays | only does anything in apply (above) |

Audits count, in every mode, what an hourly cleanup should already have
removed, a day past its window (the policy's "at the latest" is a period plus
a day):

| Audit | Counts | Window | Alarms |
|---|---|---|---|
| `url_hits_age` | `url_hits` rows | 90 days + 1 day; not applicable in apply | yes |
| `read_links_age` | `media_read_links` | 1 year (3 in apply) + 1 day | yes |
| `read_link_opens_age` | `media_read_link_opens` | 1 year (3 in apply) + 1 day | yes |
| `mail_snapshots_age` | `event_mail_snapshots` past `expires_at` | + 1 day | yes |
| `media_archived_objects` | archived Media whose object is still stored | `MEDIA_BLOB_RECOVERY_DAYS` + 1 day | no: an archived Media still used by a record keeps its object |
| `media_expired_objects` | expired Media whose object is still stored | + 1 day | no, for the same reason |

The periods are code, not configuration: the policy text names them, and a
change is a new rule version (and `RuleSetVersion`). Rules are added in
`internal/retention/rules.go` and nowhere else; the record, metrics and alarm
follow.

Contact consents ([`contact-consents.md`](contact-consents.md)), from the
consent package's own durations:

| Rule | Selects | Period | Action |
|---|---|---|---|
| `consent_pending` | a grant never confirmed: still pending, or ended before it was (`superseded` by a verified grant, or withdrawn while pending). It was never consent, so it is no proof. Counted from its last confirmation mail (its link works that long), else from when it was given | `consent.PendingTTL`, 30 days | row deleted |
| `consent_renewal_unanswered` | an open, confirmed grant whose standing renewal question went unanswered: asked before the cutoff, and no renewal and no check-in since (`consent.RenewalQuestionSQL`). A question older than the last renewal or check-in was answered by it and is void: it never ends the grant, the audience does not show it, and the grant is asked again once it is due again | `consent.RenewalAnswerWindow`, 60 days after the question | ended as `expired` (`ended_via=renewal_unanswered`), address cleared; the row stays as proof |
| `consent_proof` | a grant once confirmed that has ended (withdrawn or expired); a never-confirmed one goes with `consent_pending` | `consent.ProofRetention`, 3 years after its end | row deleted |

Once `consent_pending` has deleted a never-confirmed row, the links in its
confirmation mail find nothing: the confirm link says so, the withdraw link
has nothing left to end. Every invitation carries the withdraw link of a
confirmed grant, which stays as long as the grant (and then as proof).

## Schedule, lock and periods

- Every replica looks once an hour (`retention.CheckInterval`). The run is due
  when no scheduled full run of the configured mode ended `ok` or `partial`
  in the last 23 hours, read from `retention_runs`: a restart or a release
  never makes a run due, and runs stay at most a day apart. A run by hand
  does not count: it never stands in for the schedule's day.
- One run at a time across replicas: a run holds a session advisory lock
  (`pg_try_advisory_lock`) on its own connection, which it closes at the end
  (so a crash releases it too). A run that finds it taken records
  `skipped_locked` and does nothing; the command exits 3.
- A run still marked `running` when the next one takes the lock was left by a
  process that went away; it is marked `abandoned`.
- **Periods** are `PERIODIC_DESTRUCTION_INTERVAL` long (default 90 days, at
  most 184; read whether or not the erasure worker is on). The first run opens
  one. The first run after a period's end closes it, writes its mode and
  totals (successful full apply and dry runs, rows changed) and opens the
  next one where it ended; periods with no run in them are closed with
  zeros. `apply_runs` and `dry_runs` are the schedule's successful full runs;
  runs by hand are in the record but not in these counts (`rows_changed`
  counts every run's changes). The closed periods are the destruction record
  (`retention_periods`, with the runs and per-rule counts that point to
  them).
- **A period's mode decides what meets it.** An *apply period* needs a
  successful full apply run; a *dry-run period* (the rollout) a successful
  full run of either mode. A period is an apply period when one of its
  scheduled runs was an apply run (the schedule runs in the configured mode,
  failed runs included), or, when the schedule did not run in it at all, when
  core is in apply mode as it closes; otherwise it is a dry-run period. A
  successful apply run by hand does not meet an apply period. So the
  rollout raises no alarm, switching to apply does not alarm on the dry-run
  period before it, and a period in apply mode with only dry runs (or failed
  apply runs) alarms.

## Batches and the brake

- Apply changes at most 500 rows per statement, each in its own transaction
  under a 30-second statement timeout, with `FOR UPDATE SKIP LOCKED`: a row
  another transaction holds waits for the next run. A run that stops midway
  loses only its current batch; the next run carries on. Every rule leaves
  out the rows it has handled, so a second run changes nothing.
- A run never changes more rows than its own count found at the start.
- **The brake:** a rule that would change more than 50,000 rows, or more than
  a fifth of its table once it would change more than 1,000 rows
  (`BrakeShareFloor`), is refused: apply changes nothing for it
  (`refused_large`), the run ends `partial`, and the alarm fires. The dry run
  says `would_refuse_large`. Below 1,000 rows the share is not checked: a
  normal day's work can be most of a small table (one large Event's guests
  90 days on, 401 phones of 2,000 Tickets; a young consent table's first
  expiries), and refusing it would alarm on every such day. So a wrong rule
  can change at most 1,000 rows of a table in a day before the share stops
  it; the dry-run rollout, the record and `overdue` are what catch a smaller
  mistake. A backlog (the first apply, or a refused large day) is cleared on
  purpose, one rule at a time, from the command line:
  `core-backend retention-sweep --apply --allow-large --rule NAME`. The
  schedule never passes the brake.

**Index for the clicks.** `url_hits` keeps its rows in apply mode, so it
only grows. `url_hits_personal_at_idx` (migration `20261005140000`) indexes
by time the rows that still hold something personal, with
`url_hits_scrub`'s own predicate (`hitPersonalSQL`; a test compares the
migration with the rule and checks the plans read the index): a scrubbed
row leaves the index, and a day's count and scrub read a day's rows however
many clicks are kept. It is built with `CREATE INDEX CONCURRENTLY`, which
never blocks a click (the table takes `SHARE UPDATE EXCLUSIVE`; reads and
writes go on), as the only statement of its migration file (it cannot run in
a transaction). A build that stopped midway leaves an invalid index; the
next start drops it (concurrently) and builds it again
(`migrate.concurrentIndexes`). A change of the rule's predicate needs a new
index.

## Record

`retention_runs` (one per run: mode, trigger, full or one rule, the brake
override, rule set version, times, status, error code),
`retention_run_rules` (one per rule and run: rule, version, kind, action,
table, cutoff, `matched`, `changed`, `related_changed`, `overdue`,
`anchorless`, `table_rows`, status, error code) and `retention_periods`.

- No personal data: no address, name, IP, subject, Ticket, Event or row id,
  no free text. Rule, table and error code are identifiers the table checks
  hold to a fixed shape; an error is recorded as its SQLSTATE
  (`sqlstate_57014`), never its message, which can quote a value.
- Kept at least three years (KVKK deletion regulation art. 7(3)): no code
  path deletes these rows (`TestNoCodePathDeletesRetentionRecords`), nothing
  cascades into them, and the down migration refuses while any exists.
- Log lines carry the same: `retention_rule rule=guest_phone version=1
  mode=apply status=ok cutoff=… matched=3 changed=3 related=0 overdue=0
  anchorless=0 code=-`, `retention_run …`, `retention_period_closed …`.

## Metrics and alarm

On `/v1/metrics` while the mode is on, read from the record (not kept by the
process, so every replica reports the same and a restart loses nothing), once
an hour. Everything that alarms (the latest run's rule states and counts,
the last success, staleness, a period's runs) is read from the **schedule's**
runs only: a run by hand is recorded (`triggered_by=cli`) but neither hides
what the schedule's last run found (a dry run to look after a refused apply
leaves `refused_large` and `overdue` standing) nor stands in for a run the
schedule did not make. The change and failure counters count every run.

| Metric | Meaning |
|---|---|
| `skylab_retention_mode{mode}` | 1 for the configured mode |
| `skylab_retention_attention` | how many states need a person (below); the alarm reads this |
| `skylab_retention_last_success_timestamp_seconds{mode}` | start of the latest full run that ended ok or partial |
| `skylab_retention_period_seconds_left` | until the open period ends |
| `skylab_retention_rows_matched{rule}`, `skylab_retention_overdue_rows{rule}`, `skylab_retention_refused_large{rule}`, `skylab_retention_rule_status{rule,status}` | the latest full run |
| `skylab_retention_rows_changed_total{rule,table}` | every apply run's changes (the related table separately) |
| `skylab_retention_rule_failures_total{rule}` | failed rule runs |

Attention (each logged once per state as `retention_attention reason=…
rule=…`):

- `stale`: no successful full run in the configured mode for 48 hours,
  counted from its last success, from the switch to the mode when that came
  later, or from the first run when none succeeded (a deploy does not reset
  it);
- `run_failed`: the latest run stopped before its end;
- `rule_failed`, `refused_large`: in the latest run;
- `overdue`: rows a day past their period remain after an apply run's rule,
  or an alarming audit counts any (its hourly cleanup is not working);
- `period_without_run`: the last period closed without the run its mode
  needed (above): an apply run for an apply period, any run for a dry-run
  period.

The server alarm (`ops/wizards/erasure/erasure-alarm.sh` pattern: read the
metrics through the container's loopback, mail `/ADMIN` directly over SMTP)
reads `skylab_retention_attention`; wiring it is an ops step.

## Command

Inside the core container (it has the environment; the rules' windows follow
`RETENTION_SWEEP_MODE` as the server's do):

```
core-backend retention-sweep                       # dry run: counts per rule, changes nothing
core-backend retention-sweep --rule guest_phone    # one rule (not the day's run)
core-backend retention-sweep --apply               # changes the rows, under the brake
core-backend retention-sweep --apply --allow-large --rule url_hits_scrub
                                                   # one rule past the brake, for a backlog
```

It prints one row per rule (`RULE V KIND ACTION TABLE CUTOFF MATCHED CHANGED
RELATED OVERDUE ANCHORLESS TABLE_ROWS STATUS`), the periods it closed and the
run's status, counts only, and records the run as `triggered_by=cli`. Exit
codes: 0 done (a dry run: counted), 1 a rule failed or was refused (or the
run failed), 2 usage or configuration, 3 another run holds the lock.

**After restoring a backup**, run `core-backend retention-sweep --apply` once:
the restore brings back rows past their period, and the rules are set
operations on age, so one run removes them again.

## Rollout

1. Release with the mode unset (`off`): the migration creates the three
   empty record tables; nothing else changes.
2. `RETENTION_SWEEP_MODE=dry-run` and redeploy. Read the next day's counts:
   `core-backend retention-sweep` in the container prints the same table, or
   read `retention_run_rules` of the latest run. Check that `matched` fits
   expectations per rule, `anchorless` is explained, no audit alarms, and
   which rules say `would_refuse_large`.
3. Yusuf decides when to apply. First `core-backend retention-sweep --apply
   --allow-large --rule NAME` for each rule the dry run says
   `would_refuse_large` about, then `RETENTION_SWEEP_MODE=apply` and
   redeploy.
4. Rollback: `RETENTION_SWEEP_MODE=dry-run` (or `off`) and redeploy. Emptied
   fields do not come back (backups hold them for 31/90 days, ADR-0053); the
   hourly cleanups return to 90 days and a year.
