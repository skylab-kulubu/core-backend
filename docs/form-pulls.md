# Pulling a form's answers from Forms

Core reads a form's accepted answers from Forms' internal list and writes
their Tickets by the one Ticket rule ([form-response-tickets.md](form-response-tickets.md)),
the rule the HTTP report and the `core.forms` events take
([integration-events.md](integration-events.md#coreforms-forms-events)). Two
things ask for it (hub SPEC §10 eki, R13 eki; rabbitmq-olaylar 18):

- **The gathering (`collect`).** A form linked to an Event after people had
  answered it: core keeps no answer of a form no Event listed
  (`not_listed`). Linking the form (or first saving its mapping), and the
  organizer's button below, queue a gathering of every answer that is not
  archived, kept as Unticketed respondents; **it writes no Ticket** (Yusuf,
  2026-11-25, K1: a Ticket is always the organizer's choice, sent from the
  list; this replaces 2026-11-22's "only the button pulls, and writes
  Tickets"). Its rules are in [form-respondents.md](form-respondents.md#gathering-a-linked-form).
- **The daily catch-up.** Once a day for each form the current Events list,
  the answers changed since the last pull: whatever an event (or the HTTP
  report) missed (ADR-0071 karar 6).

Code: `internal/formsfeed` (`Puller`, `Backfills`, `PostgresPulls`), wired
in `cmd/core-backend`. An Event's form mapping
([form-mappings.md](form-mappings.md)) names the questions whose answers
the reads ask for (`fields`).

## Backfill

```http
POST /v1/events/{id}/forms/{formId}/backfill
Authorization: Bearer <the organizer's token>
```

No body. It queues the form's gathering (`collect`,
[form-respondents.md](form-respondents.md#gathering-a-linked-form)); before
bilet 04 it queued a pull that wrote Tickets. The caller is a person who may edit the Event (a product's
service account never asks). Then, in order:

| Answer | When |
| --- | --- |
| `404` | no such current Event (archived too), or a draft the caller may not edit |
| `403` | the caller may not edit the Event |
| `409` `event_cancelled` | the Event is cancelled |
| `409` `event_ended` | the Event has ended |
| `409` `form_not_listed` | the Event does not list the form as its application or an extra form (a feedback form is never pulled) |
| `409` `form_in_use` | another current Event lists the form, as any form (the one-place rule, ADR 0069 addendum of 2026-11-22): last year's form linked to this year's Event must not ticket its applicants through the other |
| `429` `backfill_cooldown` | the form's backfill was asked for less than 15 minutes ago (`Retry-After`: the seconds left); a button pressed again and again |
| `503` `forms_pull_off` | `EVENTS_FORMS_ENABLED` is not `true` (below) |
| `202 {"queued": true}` | a job is queued |
| `200 {"queued": false}` | a gathering of the form waits already |

The request writes one line, `"event":"form_backfill_request"`, with the
Event, the form and `outcome` (`queued`, `waiting`, `cooldown`, `refused`),
never who asked. The ask is kept on the form (`last_backfill_at`); a form
the daily pass had not seen gets its watermark then.

The gathering reads every answer of the form, any status, and keeps rows,
never a Ticket. What follows is the daily catch-up's way, as the backfill
did before bilet 04. Each
answer takes the Ticket rule as the HTTP report's does: on every current
Event that lists the form, not cancelled, not ended, open to applications
when the answer was sent (`submittedAt`), within capacity or on the waiting
list; an answer that already has its Ticket keeps it. A cancelled Ticket
takes the forms report's one rule (`Admission.FormResponse`,
[form-response-tickets.md](form-response-tickets.md)): only one Forms' own
decline cancelled is reopened by an accepted answer; one the holder or an
organizer cancelled stays cancelled and nobody is mailed. The same holds
for the daily catch-up overlapping an event or the HTTP report. The rule reads the
Events at each answer, so an Event cancelled while the job runs gets no more
Tickets. A Ticket written this way is mailed as any other
([ticket-created-notification.md](ticket-created-notification.md)): at once on
a published Event, at publishing on a draft.

## Daily catch-up

Hourly, one replica (an advisory lock) reads the forms of the current Events
that are neither cancelled nor ended (drafts too), their application and
extra forms, and keeps a row for each in `form_pull_watermarks`:

- A form seen the first time gets `watermark` = now and its first pass a
  day later: the pass never reaches answers from before the form was seen
  linked (those are the gathering's).
- A form whose pass is due (`next_catchup_at`) gets a catch-up job that asks
  for the answers changed since `watermark − 5 minutes` (a margin for an
  answer committed at the watermark's time, or clocks a little apart; a
  repeat is harmless), and its next pass a day later. With
  `EVENTS_FORMS_ENABLED=true` it asks for every status (no `status`
  filter): a pending or declined change an event missed reaches the
  Unticketed respondents and K6's mark ([form-respondents.md](form-respondents.md)).
- A form no current Event lists keeps its row 30 days, then loses it.

## The job

A worker looks every 30 seconds for a due job (`FOR UPDATE SKIP LOCKED`,
any replica) and leases it for 15 minutes. It reads the list a page at a
time:

```http
GET {FORMS_INTERNAL_URL}/internal/v1/forms/{formId}/responses?status=accepted&changedSince=…&after=…&limit=100
Authorization: Bearer <core's service account token: aud ∋ forms, skyforms:responses:read-service>
```

answering `{"items": [forms.response.created's data], "nextCursor", "watermark"}`
(SPEC §10 eki). An item that does not read, or names another form, is
skipped and counted. A guest's answer (`userId` null, `hasGuest`) is read
from the single answer endpoint for the guest's name and e-mail, as for an
event. Each answer runs in a transaction of its own and keeps its version
(`forms_record_versions`): a version older than one core handled is stale;
the same version is taken again (an event of a form not linked then was
`not_listed`). The next page's cursor is kept after each page, with the
lease renewed, so a retry goes on from there. A cursor that does not move,
or a list past 1,000 pages in one run, stops the job with an error
(`reason` `invalid`) instead of looping.

- Done (the last page, `nextCursor` null): the job is deleted and the form's
  watermark moves forward to the page's `watermark` (never back); when Forms
  sends none, to when the job's run started, so the next window does not
  grow.
- An answer the single read did not find (`404`) or the Ticket rule refused:
  counted, the job goes on.
- Anything else (Forms down, `401`, `403`, `404` of the list, which is a
  form archived or deleted, or a Forms without the route yet; the database):
  the job is tried again from its page, after 1 minute doubling to a day at
  most; after a week from its request it is given up (`abandoned`). Core
  ships first: until Forms has the list, each form's job waits a day
  between tries and writes a line each time.

Each run of a job writes one line, `"event":"form_pull"`: `kind`
(`collect`, `catchup`; a `backfill` job from the release before runs and
counts as `collect`), `form_id`, `event_id` (a gathering's), `outcome`
(`done`, `retry`, `abandoned`), `reason` (`not_found`, `forbidden`,
`invalid`, `failed`), `attempts`, `pages`, `items`, `skipped`,
`tickets_written`, `failed` and `outcomes` (the count of each answer's
outcome); never who answered. Each answer also writes its `form_response`
line with `via` `pull`.

## Off and on

The pulls share core.forms' switch, `EVENTS_FORMS_ENABLED` (Yusuf,
2026-11-24: one switch of its own for Forms' integration; the SPEC had
proposed no new variable). With `true` they run, whether or not
`EVENTS_ENABLED` is on (the list is HTTP, apart from the broker), and core
needs Forms' internal address (`FORMS_INTERNAL_URL`, else
`ACCOUNT_ERASURE_FORMS_URL`) and its own service account (`KEYCLOAK_URL`,
`KEYCLOAK_REALM`, `KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET`), or it
stops naming what is missing. Off (the default) nothing is planned or
pulled, and the backfill answers `503 forms_pull_off`.

## Tables

Migration `20261124161000` (expand only):

| Table | Rows | Kept |
| --- | --- | --- |
| `form_pulls` | one job: `form_id`, `kind` (`collect` since migration `20261127170000`), `event_id` and `requested_by` (a gathering's Event and, for the button, the organizer's subject), `requested_at`, `changed_since` (a catch-up's), `cursor`, `attempts`, `next_attempt_at`; one job of each kind a form | until done, at most a week |
| `form_pull_watermarks` | one form: `watermark`, `next_catchup_at`, `last_backfill_at`, `updated_at` | while an Event lists the form, then 30 days |

Ids and times only; `requested_by` is a person's subject for at most a week.

## Metrics

`/v1/metrics`: `skylab_form_pulls_total{kind="collect|catchup",outcome="done|retry|abandoned"}`
(`kind="backfill"` before bilet 04),
and the answers' `skylab_forms_answers_total{via="pull",outcome}` and
`skylab_forms_answer_reads_total{result}` (shared with `core.forms`).

## Against the standard

Matches: Stripe's List Events for a webhook consumer that missed events,
the same objects as the events; Microsoft Graph's delta query (`nextCursor`
as `@odata.nextLink`, `watermark` as `@odata.deltaLink`); a Postgres work
queue with `SKIP LOCKED` and leases for in-service jobs (ADR-0071 karar 9).
Deviations: the watermark is a time, not an opaque token (SPEC §10 eki: no
new table in Forms); a form's first watermark is when the daily pass first
saw it linked, not the linking itself (core keeps no link time; the pass
runs hourly, and answers in between come as events or the HTTP report).

## Rolling back

The code before pulls nothing; the jobs and watermarks stay unread. The
migration's down drops both tables. The panel's button, if shipped, would
then answer `404`.
