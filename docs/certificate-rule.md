# Certificate rule

Sertifika × oturum katılımı (spec `.scratch/sertifika-oturum/SPEC.md` in the hub; Yusuf's decisions of 2026-11-23). An Event's certificate rule reads "at least M of the N counted Sessions": the organizer picks which Sessions count, which are required, and M.

Tickets 01 (the data, the rule endpoints, the pure decision) and 02 (eligibility over the marks in a fixed number of queries, the attendance list, the manual issue's justification).

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
