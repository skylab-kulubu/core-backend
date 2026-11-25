# Unticketed respondents

An **Unticketed respondent** is a person who answered the form an Event lists
(its application or an extra form), whose answer Forms has not declined, and
who holds no Ticket on that Event yet. The panel shows them on the
Katılımcılar tab as "Bilet gönderilmedi" (accepted in Forms) or "Forms'ta
onay bekliyor" (pending in Forms); the organizer sends their Tickets one by
one or all at once. A respondent is not a Ticket: no capacity, door,
attendance, certificate or mail. Once the person holds a Ticket on the Event,
the row goes (the Ticket keeps the answer, `form_response_id`).

Decisions: hub SPEC form-alan-eslestirme §6a, §7 and K1–K6 (Yusuf,
2026-11-25). Code: `internal/formsfeed` (`Respondents`, `Collects`,
`TicketJobs`, `PostgresRespondents`, `PostgresTicketJobs`), the Ticket write
`ticket.Service.AdmitFormRespondent`.

## Off and on

Everything that reads Forms rides `EVENTS_FORMS_ENABLED` ([form-pulls.md](form-pulls.md#off-and-on)):

- **Off (the default):** answers take the one Ticket rule alone, exactly as
  before ([form-response-tickets.md](form-response-tickets.md)); no row is
  written and no gathering is queued; `formResponsesAutoTicket` is stored
  but not read. The list and the sending routes still answer (they read no
  Forms), over rows written while it was on.
- **On:** every answer of a linked form goes through the routing below, and
  linking a form gathers its earlier answers.

## Where an answer goes

The HTTP report (`POST /v1/forms/{formId}/responses`), a `core.forms` event
and a pull take the same way (`Respondents.Route`), after the person is
resolved (`ResolveRespondents`: a signed-in person, Forms' named guest, or a
guest built from the Event's form mapping, [form-mappings.md](form-mappings.md)).
"Open Events" are the current Events listing the form that are neither
cancelled nor ended.

| Forms says | "Otomatik bilet gönder" on (default) | off | The person holds a Ticket |
|---|---|---|---|
| accepted (no review, or approved) | Ticket by the one rule | kept as an accepted row (`source` `new`) | no second Ticket; a row of theirs goes |
| pending (or flagged) | kept as a pending row | same | the Ticket stays; its `formsStatus` turns `pending` ("Forms'ta yeniden onay bekliyor"; `tickets.form_response_status`, #460) |
| pending → accepted | Ticket; the row goes | the row turns accepted | `formsStatus` back to `accepted` |
| declined → accepted again | as accepted | the Ticket takes the answer as the rule would: Forms' rejection mark goes, `formsStatus` `accepted` | same |
| Returned: a corrected answer accepted | as accepted | the Ticket is taken over by the new answer (same form, the old one no longer accepted; #460) | same |
| declined, archived, returned | the answer's rows go | same | the Ticket is marked `formsRejectedAt`, not cancelled (K6, [form-response-tickets.md](form-response-tickets.md)) |

- With the setting on, an accepted answer that still earns no Ticket (the
  Event full with its waiting list off, applying closed when it was sent) is
  kept as an accepted row too: the organizer may send it, since closing
  applying stops only the automatic way.
- An answer with no usable person (no e-mail address, no name: `mapping_incomplete`,
  `no_respondent`; a signed-in person core cannot find, or whose account is
  blocked or erased) keeps no row. A signed-in person's account row is made
  first if core has none yet (as the rule does before their Ticket): the
  row's `user_id` names only an active account (database guard).
- With the setting off the rule does not run on that Event, so a Ticket the
  person already holds there takes the accepted answer by the rule's own
  step (`ticket.Service.KeepFormRespondent`, #460's `KeepFormResponse`).
- Turning the setting on later sends nobody by itself: the rows wait for the
  organizer.
- A form listed by more than one current Event (from before the one-place
  rule) is decided per Event: each Event's setting, and a mapped guest only
  on the Event whose mapping built it (`ticket.FormResponse.OnlyEvents`).

The answer's log line is the usual `form_response` (outcome
`respondent_kept` when only a row was written); routing writes
`"event":"form_respondent"` lines with the step, ids and a count, never who.

## Gathering a linked form

When an Event newly lists a form (create or update; `event.FormsLinkedHook`)
or first saves the form's mapping, core queues a **collect** job
(`form_pulls.kind = 'collect'`, one waiting job a form). Not for an archived,
cancelled or ended Event, nor for a form another current Event lists
(`form_in_use`). The job reads every answer of the form that is not archived
(no `status`, no `changedSince`; the mapping's `fields`) and, for each:

- declined: the answer's row goes;
- a guest's answer (`hasGuest`): read from the single answer endpoint for
  the name and e-mail;
- the person holds a Ticket on the Event (the answer's, the account's, or
  the guest address's; a Ticket Forms' own decline cancelled does not
  count): nothing kept;
- otherwise kept as a row, `source` `collect`, `formsStatus` as Forms says.

It writes no Ticket and moves no watermark: answers after the link reach the
routing above (event, HTTP report, daily pass). A repeat finds the same rows.

Asked again while one waits or runs (the mapping first saved during the
gathering, the button), the job is marked to run again from its first page:
`requested_at` moves and the cursor goes; the running pass keeps neither
its saved page nor its end (both check `requested_at`), and the job runs
again at once. One waiting out a failure's back-off is due at once.

A full pass (from the first page) keeps the counts of answers that name
nobody usable on the Event's form (`event_form_collect_counts`, K3):
`missingName` (no first and last name, a one-word full name),
`missingEmail` (no plain address), `noPerson` (no sign-in, no named guest,
no mapping); the next full pass replaces them.
The job's line is the usual `"event":"form_pull"` with `kind` `collect`;
outcomes `kept`, `ticketed`, `declined`, `gone`, `no_respondent`,
`mapping_incomplete`, or why the job did nothing (`event_gone`,
`event_cancelled`, `event_ended`, `not_listed`, `form_in_use`).

`POST /v1/events/{id}/forms/{formId}/backfill` now queues the same
gathering (its refusals and 15-minute cooldown as before,
[form-pulls.md](form-pulls.md#backfill)); it no longer writes Tickets
(Yusuf, 2026-11-25: K1 of 2026-11-22 changes, a Ticket is always the
organizer's choice). A `backfill` job left from the release before runs as a
gathering.

## The routes

The routes take the Event's Ticket rights, as its Ticket routes do (authz
on the Event's Tickets, its Co-organizers included; #456): the list asks
`Read` (who may see the Event's Tickets), the preview, the sending and a
job ask `Assign` (who may add attendees), so a Co-organizer team that only
lets its members see the Event sends no Ticket. A product's service
account never may. `404` for no such current Event or a draft the caller
may not see, `403` otherwise. Errors are RFC 9457 problem details with
`code`.

### List

```http
GET /v1/events/{id}/respondents?formId=&formsStatus=accepted|pending&q=&after=&limit=
```

`200`, `Cache-Control: no-store`:

```json
{
  "items": [
    {"id": "…", "formId": "…", "responseId": "…", "userId": null,
     "firstName": "Ada", "lastName": "Lovelace", "email": "ada@example.com",
     "submittedAt": "2026-11-20T10:00:00Z", "formsStatus": "accepted", "source": "collect"}
  ],
  "nextCursor": null,
  "total": 1,
  "collecting": false,
  "incomplete": {"missingName": 0, "missingEmail": 0, "noPerson": 0}
}
```

- Ordered by `submittedAt` (none last), then `id`; `after` is the previous
  page's opaque `nextCursor`; `limit` 1–200, default 50.
- `q` searches a guest's name and e-mail (case folded, at most 100
  characters). A signed-in row (`userId` set) keeps no name; the answer fills
  `firstName`, `lastName`, `email` from core's account (empty for a person
  erased or blocked), and `q` does not search them.
- `collecting` is true while a gathering of the Event's forms (of `formId`)
  waits or runs, not one waiting out a failure, and only with
  `EVENTS_FORMS_ENABLED=true`: the panel says "Form yanıtları alınıyor".
- `incomplete` sums the last full gatherings' counts (above) of the
  Event's forms (of `formId`): the panel's "eksik ad" line.
- `400 respondents_invalid`: an unknown `formsStatus`, a bad `formId`,
  `limit` or cursor.

### Preview

```http
POST /v1/events/{id}/respondents/tickets/preview
{"ids": ["…"]}            or            {"all": true, "formId": "…"}
```

`200 {"tickets": 1, "waitlisted": 2, "full": 0, "alreadyTicketed": 0, "pendingSkipped": 1}`:
of the accepted rows selected whose person holds no Ticket, how many get an
active Ticket within the Event's capacity, how many go on the waiting list
(on) or find no place (off); `alreadyTicketed` rows whose person holds one;
`pendingSkipped` rows pending in Forms (never sent). A count, not a promise.
`ids` are 1–200 of the Event's rows; `all` takes every row of the Event, of
`formId` when given (`formId` is read with `all` only).

### Send

```http
POST /v1/events/{id}/respondents/tickets
```

The same body.

- `ids` (at most 200): sent now, `200 {"recorded", "waitlisted", "full", "skipped"}`.
- `all: true`: a background job, `202 {"jobId"}`. One waiting job an Event
  and form (or the whole Event): the same request again answers the same
  `jobId`. The job waits while a gathering of its forms runs, then sends
  every accepted row, a page at a time.

Each person takes the one Ticket rule (`ticket.Service.AdmitFormRespondent`,
the forms report's write on one Event): REGISTERED for a signed-in person,
a guest Ticket for a guest (an existing guest is never renamed),
`Admission{Waitlist: true, FormResponse: responseId}`, the capacity and the
waiting list as always; the same person never gets a second Ticket.
Closing applying does not refuse it (the organizer is adding the person). A
row whose person holds a Ticket afterwards goes (`recorded`, `waitlisted`,
or `skipped` for one held already); a full Event's row stays (`full`); a
pending row is `skipped` and stays; an id that is not the Event's row is
`skipped`. Sending the same ids again writes nothing: their rows went. The
Ticket's mail goes as any other's ([ticket-created-notification.md](ticket-created-notification.md)):
at once on a published Event, at publishing on a draft.

Each request writes a Team audit row ([team-audit.md](team-audit.md)), as
an attendee import's: action `ticket.import` (the Event, no one Ticket),
basis privileged, else the Leader, else the Event's editor, with the counts
(`rows`, `created`, `waitlisted`, `full`, `existing`, `members`, `guests`;
for `all: true` the rows selected, the job's own counts on the job). With
the record on and no row, the request is refused before anything is
written: `503 audit_unavailable`.

`409 event_cancelled`, `409 event_ended`: the Event takes no Ticket now.
`400 respondents_invalid`: neither 1–200 `ids` nor `all: true`.

### A job

```http
GET /v1/events/{id}/respondents/ticket-jobs/{jobId}
```

`200 {"jobId", "formId", "state": "queued|running|done|failed", "recorded", "waitlisted", "full", "skipped", "createdAt", "updatedAt"}`,
`404 ticket_job_not_found`. A job goes on after the last row it finished,
so a retry counts each row once. One that fails is tried five times, then
`failed`; one whose Event was cancelled or ended meanwhile is `failed` at
once. A finished job is kept 30 days. The worker runs whatever
`EVENTS_FORMS_ENABLED` says; each run writes `"event":"respondent_ticket_job"`
with ids and counts.

## The Event's setting

`formResponsesAutoTicket` (panel: "Otomatik bilet gönder") on the Event's
read and write bodies: `true` by default (the migration gives every Event
`true`, as before), `null` on a patch is refused (`400`), left out keeps it.
A copy keeps it ([event-copy.md](event-copy.md)).

## The Ticket's Forms status

A Ticket's answer adds `formsStatus` (`accepted`, `pending`, `declined`; left
out for a Ticket no forms answer wrote) beside `formsRejectedAt`, for the
panel's two badges (Forms status and Ticket status are apart). It reads
`tickets.form_response_status` (K6, #460; `declined` whenever
`formsRejectedAt` is set, `accepted` for an answer kept before that column).

## Data

Migration `20261127170000`:

| Table / column | What | Kept |
| --- | --- | --- |
| `event_form_respondents` | `event_id`, `form_id`, `form_response_id` (one row an answer on an Event), `user_id` for a signed-in person (then no name or address), else `first_name`, `last_name`, `email`; `submitted_at`, `forms_status`, `source`, `created_at` | until the person holds a Ticket, the answer is declined, the Event lets the form go (same transaction), account erasure, or two years after the Event's end (retention rule `form_respondents`) |
| `respondent_ticket_jobs` | a send-all job: Event, form, the organizer's subject, state, counts, the last row handled | 30 days after it finished; account erasure clears the subject |
| `event_form_collect_counts` | an Event's form: the last full gathering's `missing_name`, `missing_email`, `no_person` | with the Event; the form let go takes it |
| `events.form_responses_auto_ticket` | the setting | with the Event |
| `form_pulls.kind = 'collect'` | the gathering | until done, at most a week |

Never an answer's text, an unmapped answer or anything else of Forms.

- **Retention:** rule `form_respondents` ([retention-sweep.md](retention-sweep.md)):
  the Event's end + 2 years (as `guest_identity`), an Event without a date
  from the row's `created_at`; the row is deleted. It runs in the sweep's
  mode, dry-run first.
- **Account erasure:** `anonymize_core` deletes the person's rows, their
  account's and those of any of their addresses, and its proof keeps
  `{"form_respondents_deleted": n}` ([account-lifecycle.md](account-lifecycle.md#service-erasure-steps));
  the person's send-all jobs and form pulls forget who asked
  (`requested_by`).

## Rolling back

**Before going back to the release before, turn `EVENTS_FORMS_ENABLED` off
(and redeploy), then roll the image back.** That release runs every
`form_pulls` job it finds, a waiting `collect` job as a backfill that
writes Tickets; with the switch off it claims none.

The release before neither reads nor writes any of it. The migration's down
keeps every row and column; it puts back `form_pulls`' checks without
`collect` (`NOT VALID`, so a waiting job stays) and parks a waiting collect
job (`next_attempt_at = 'infinity'`), because the release before would run it
as a backfill and write Tickets; up again puts it back. This release never
moves a parked job (its cursor, retry and finish keep `infinity`).
