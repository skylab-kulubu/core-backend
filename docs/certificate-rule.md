# Certificate rule

Sertifika × oturum katılımı (spec `.scratch/sertifika-oturum/SPEC.md` in the hub; Yusuf's decisions of 2026-11-23). An Event's certificate rule reads "at least M of the N counted Sessions": the organizer picks which Sessions count, which are required, and M.

This page is ticket 01: the data, the rule endpoints and the pure decision. Eligibility itself still counts every live Session and none is required until ticket 02 moves it to one query over the marks.

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
