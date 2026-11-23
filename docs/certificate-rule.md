# Certificate rule

Sertifika × oturum katılımı (spec `.scratch/sertifika-oturum/SPEC.md` in the hub; Yusuf's decisions of 2026-11-23). An Event's certificate rule reads "at least M of the N counted Sessions": the organizer picks which Sessions count, which are required, and M.

Tickets 01 (the data, the rule endpoints, the pure decision), 02 (eligibility over the marks in a fixed number of queries, the attendance list, the manual issue's justification) and 03 (automatic finalization, early manual finalization, rescan).

## Kinds

`events.attendance_rule`:

| Rule | A Ticket is eligible when |
|---|---|
| `none` | never: no certificate |
| `once` | N ≥ 1, every required Session checked in, and at least 1 counted check-in |
| `count` | N ≥ 1, every required Session checked in, and counted check-ins ≥ the effective threshold |
| `ratio` | (old) N ≥ 1, every required Session checked in, and check-ins / N ≥ the ratio, compared in floats as before |

- **N** is the live counted Sessions: on a current day, not archived, not cancelled, `certificateCounted`. N = 0 issues nothing.
- **Threshold.** `attendanceMinCount` null is the default, ⌈3N/4⌉, computed in integers as `(3N + 3) / 4` (N = 8 → 6, 4 → 3, 3 → 3). A set M applies as min(M, N): cancelling or uncounting Sessions never makes the rule impossible. M below 1 is refused; M above N is kept.
- **`ratio` is kept, not written.** An Event that has it keeps it through every write that leaves the rule alone or sends it back unchanged (sky-app sends the whole Event). Choosing it, or a new ratio, is `400`. The panel offers to turn it into `count`. The down migration turns `count` back into `ratio` at 0.75.
- The threshold belongs to `count` alone: any other rule stores none.

## Session marks

On every Session JSON:

| Field | Meaning |
|---|---|
| `certificateCounted` | one of the N. Default `true`; a new `NETWORKING` Session defaults to `false` (`event.CountedByDefault`, one place to change) |
| `certificateRequired` | missing it refuses the certificate. Default `false`. A required Session is always counted (database check `sessions_required_is_counted`) |

`POST /v1/sessions` and `PUT /v1/sessions/{id}` take both, optional: left out on update keeps the stored mark (the type changing does not move it). Required without counted is `400`. A copy ([event-copy.md](event-copy.md)) carries both marks and `attendanceMinCount`.

## Endpoints

`GET /v1/events/{id}/certificate-rule`, for whoever may edit the Event or read its certificates:

```json
{"rule": "count", "minCount": null, "effectiveMin": 6, "countedTotal": 8,
 "sessions": [{"id": "…", "dayId": "…", "title": "Açılış", "sessionType": "TALK",
   "startTime": "…", "endTime": "…", "cancelled": false, "counted": true, "required": true}]}
```

`ratio` is there only on a `ratio` Event, and `effectiveMin` is then the fewest check-ins its ratio takes. `sessions` lists the current Sessions on current days, cancelled ones too, in the program's order.

`PUT /v1/events/{id}/certificate-rule`, for whoever may edit the Event (who writes `attendanceRule` with `PATCH /v1/events/{id}`):

```json
{"rule": "count", "minCount": 6, "sessions": [{"id": "…", "counted": true, "required": false}]}
```

One transaction: the rule, the threshold, and the listed Sessions' marks, all or nothing. A Session it does not list keeps its marks. `400` for `ratio` or an unknown rule, `minCount` below 1, a Session listed twice, one without both marks, required without counted, and a Session that is not a current Session of the Event on a current day (another Event's, archived). It answers what `GET` answers.

`POST /v1/events` and `PATCH /v1/events/{id}` take `attendanceMinCount` too (null for the default); the Event JSON leaves it out when there is none.

## Eligibility

`certificate.AttendanceStore.EventAttendance` reads an Event's attendance in two queries, however many Tickets it has: the live Sessions with their marks, then every Ticket with its counted check-ins (`GROUP BY ticket_id`), its missing required Sessions, its name, its active and latest revoked certificate and its latest job (`LEFT JOIN`s). The Summary, attendance finalization, the manual issue and the list all decide from it with `certificate.Eligible`; no read per Ticket is left. A store without the query (the memory stores of the tests) reads the program once per Event from the other stores.

A Session written straight to a store (`event.Session{}`, past the service's defaults) is **not counted**: the Postgres store writes the mark it is given. Test fixtures that expect a Session to count set `CertificateCounted: true`; `TestUncountedFixtureIssuesNothing` keeps the trap visible.

## Attendance list

`GET /v1/events/{id}/certificates/attendance`, for whoever may read the Summary (`certificate:read` or the template workspace's read). No e-mail; the name is the Guest's on the Ticket, the account's for a member (as the certificate would name them). In order of name, no paging.

```json
{"countedTotal": 8, "effectiveMin": 6, "rule": "count",
 "required": [{"id": "…", "title": "Açılış"}],
 "rows": [{"ticketId": "…", "name": "Ayşe Yılmaz", "ticketType": "REGISTERED", "attended": 6,
   "missingRequired": [{"id": "…", "title": "Açılış"}], "status": "below",
   "certificate": {"serial": "…", "issuedAt": "…", "revokedAt": "…"} }]}
```

- `attended` counts check-ins on live counted Sessions only; `missingRequired` lists live required Sessions without a check-in, in the program's order (a cancelled or archived required Session drops out).
- `status`, first that holds: `issued` (an active certificate) > `queued` (its latest job is queued or running) > `failed` (its latest job failed) > `revoked` (only a revoked certificate) > `eligible` > `below`.
- `certificate` is the active certificate, else the latest revoked one (with `revokedAt`), else null.

`GET /v1/events/{id}/certificates/summary` adds `countedTotal`, `effectiveMin` and `belowCount` (the Tickets whose status is `below`).

## Manual issue and its justification

`POST /v1/events/{id}/certificates/issue` takes `{"ticketId": "…", "justification": "…"}`. The justification is trimmed; up to 500 characters.

- A Ticket the rule does not make eligible needs one: without it (or blank) `400` with `code: "justification_required"`.
- An eligible Ticket needs none; one given is kept all the same.
- Over 500 characters: `400` with `code: "justification_too_long"`.
- `202` with the batch, as before.

The batch (`certificate_batches.justification`, migration `20261124000000`) is the manual issue's audit record: who asked (`requestedBy`), when (`createdAt`) and why (`justification`). `GET /v1/certificate-batches/{id}` and `GET /v1/events/{id}/certificate-batches` answer it; only who reads the certificate workspace sees it, never the public verification. Rolling the migration back drops the text written since.

## Automatic attendance finalization

An hour (`certificate.AutoFinalizeDelay`) after an Event's **last live Session** ends, core closes its attendance by itself, as `POST …/certificates/finalize` does, and queues the eligible Tickets' certificates; the issuance worker issues them and sends the `core.certificate` mail as before.

- A Session's end is its end time, else its day's end, else the Event's end (the feedback mail's expression). Every live Session counts, counted or not.
- **Due** when the Event is published and not archived, its rule is not `none`, its switch `attendanceAutoFinalize` is on, its attendance is not closed, it has a live Session, and that end + 1 h is past but no more than 7 days ago (`AutoFinalizeWindow`). An older Event is left to its organizer.
- `certificate.MaintainAutoFinalize` runs a pass every minute (worker `certificate auto finalize`). A pass takes a transaction-scoped advisory lock; another replica's pass that finds it taken skips. The state row's key (`certificate_event_state.event_id`) closes an Event once: of two passes, or of a pass and the organizer's manual finalization, the second writes nothing and queues nothing.
- The system closes it: `attendance_finalized_by` and the batch's `requested_by` are null, `finalized_source` is `auto` (a manual one is `manual`).
- An Event that fails (its template cannot be resolved, a database error) is skipped, logged with its id and the failure's class, and counted in `core_certificate_auto_finalize_errors_total`; the next pass tries it again. `core_certificate_auto_finalized_total` counts the Events closed. Both are on `/v1/metrics`.
- **The publisher's mail** (S6): who published the Event (`published_by`) gets `core.certificate-auto-finalized` (`SKYMAIL_CERTIFICATE_AUTO_FINALIZED_TEMPLATE_KEY`; empty turns it off) with `FirstName`, `EventName`, `EventId`, `CertificateCount` (the certificates queued) and `OwnerTeam`. It goes by key to `/v1/mail_tasks/single` like the certificate mail: a refusal is logged, never retried, and never stops the finalization. No mail when no publisher is known or the account is gone.

**The switch.** `attendanceAutoFinalize` is on the Event JSON. `POST /v1/events` takes it (none is `true`); `PATCH`/`PUT /v1/events/{id}` turn it on or off (`null` is `400`). A copy has it on. Migration `20261124010000` turns it off on every Event that had ended when it ran (by its dates, or by its last live Session's end) and whose attendance was not closed (S3): old Events are not closed by the clock.

**Early manual finalization** (K7, S4). `POST /v1/events/{id}/certificates/finalize` opens when the Event has ended or is undated, as before, **or** its last live Session has ended, without waiting the hour. A running Session still answers `409`; a draft or a cancelled Event refuses it with its reason, as before.

**Summary.** `GET …/certificates/summary` adds `autoFinalizeAt` (last live Session's end + 1 h, while the rule issues, the switch is on, the Event is published and its attendance open, and that time is within the window) and, once closed, `finalizedSource` (`manual` or `auto`).

## Rescan

`POST /v1/events/{id}/certificates/rescan` ("Uygunları yeniden tara", K8), for whoever may issue (`certificate:issue`; else `403`).

- `409` while the attendance is open; a draft or cancelled Event refuses it with its reason.
- Queues, with `reason: "rescan"` in one batch, the Tickets eligible now that have **never had a certificate**: not one with an active certificate, not one whose certificate was revoked (a revocation is deliberate), not one whose latest job is queued or running.
- `202` with `{"queued": N, "batch": {…}}`; with no one, no batch is opened: `200` with `{"queued": 0}`.
