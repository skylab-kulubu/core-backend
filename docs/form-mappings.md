# Form mappings

**An organizer tells core which questions of an Event's form hold a guest's
name and e-mail, and optionally their phone number, university, department
and grade; core asks Forms for those answers only.** A form without
identity fields (props.identity firstName/lastName/email) makes Forms
report an answer with no `userId` and no `guest`: the Ticket rule finds
nobody (`no_respondent`). With a mapping, core builds the guest from the
mapped answers (hub SPEC form-alan-eslestirme §5).

Code: `internal/formsfeed` (`Mapping`, `Mappings`, `PostgresMappings`),
`internal/handlers/form_mapping.go`, wired in `cmd/core-backend`.

## The mapping

| Field | Question that holds |
| --- | --- |
| `firstName` | the first name |
| `lastName` | the last name |
| `fullName` | first and last name in one ("ad soyad") |
| `email` | the e-mail (required) |
| `phone` | the phone number (optional, [the details](#the-details)) |
| `university` | the university (optional) |
| `department` | the department (optional) |
| `grade` | the grade, year of study (optional) |

Each is a question id of the form (Forms' schema item id, at most 100
characters, no comma), or `null`. The rule: `email`, and either
`firstName` and `lastName`, or `fullName`; no question twice (the details'
included). One mapping
per Event and form, for a form the Event lists as its application
(`formUrl`) or an extra form (`extraFormUrls`); a feedback form has none.

## Routes

All under `/v1/events/{id}/forms/{formId}`, with the organizer's token.
The caller is a person who may edit the Event (authz `Update` on the Event,
its Co-organizers included), and on the form's team in Forms: its owner, an
editor or a viewer (Yusuf, 2026-11-26: a mapping changes nothing in the
form, only says which question names the guest, and Forms shows a viewer
the answers already; until then a viewer was refused). Someone not on the
team is refused. A product's service account never is. Each route
reads the form from Forms for its team (`collaborators: [{userId, role}]`,
forms-backend #43; removed members are left out by Forms); a Forms that
sends no `collaborators` lets nobody in.

| Route | Answer |
| --- | --- |
| `GET …/questions` | `200 {formId, title, questions: [{id, type, question, inputType, identity}], suggestion: {firstName, lastName, fullName, email, phone, university, department, grade}}` (the details are never suggested: `null`) |
| `GET …/mapping` | `200 {firstName, lastName, fullName, email, phone, university, department, grade, updatedAt}`; `404 mapping_not_found` without one |
| `PUT …/mapping` | body `{firstName, lastName, fullName, email, phone, university, department, grade}` (a detail left out is none); `201` the first time, `200` when it replaced one, with the mapping. The first save (with `EVENTS_FORMS_ENABLED=true`) queues the form's gathering, so answers that named nobody before are kept with the guests it builds ([form-respondents.md](form-respondents.md#gathering-a-linked-form)) |
| `DELETE …/mapping` | `204`; `404 mapping_not_found` without one |

`questions` comes from Forms' form read (`GET {FORMS_INTERNAL_URL}/internal/v1/forms/{formId}`,
core's service account, forms-backend #43); the team in it is not passed
on. `inputType` and `identity` are
left out when Forms has none. `suggestion` is what the form's identity
fields give (hub SPEC §11 K5, Yusuf 2026-11-25): a field one
question claims is filled, else `null`. The panel guesses the rest from
`inputType` and the question's text (SPEC §5.2); the organizer saves.

A save reads the form from Forms again and checks every id is one of its
questions.

| Refusal | When |
| --- | --- |
| `400` | an id in the path that is not a UUID, or a body that is not JSON |
| `404` | no such current Event (archived too), or a draft the caller may not edit |
| `403` | the caller may not edit the Event, or is a service account |
| `403 forms_access_denied` | the caller is not on the form's team in Forms (owner, editor or viewer), or Forms does not say who is |
| `404 form_not_listed` | the Event does not list the form as its application or an extra form |
| `400 mapping_invalid` | the mapping breaks the rule above |
| `422 mapping_question_missing` | the form has no question of some ids: `missing` lists them |
| `404 form_not_found` | Forms has no such form (deleted, or a Forms without the route yet); `DELETE` still removes the mapping |
| `503 forms_pull_off` | `EVENTS_FORMS_ENABLED` is not `true` (every route reads Forms) |
| `503 forms_unavailable` | core has no Forms address or service account, or Forms did not answer, refused core or answered what core cannot read |

With the switch off every route answers `503 forms_pull_off` once the
Event's own checks pass: each needs Forms' form read for the team check.

Each questions read, save and delete writes one line,
`"event":"form_mapping"`: `action` (`questions`, `put`, `delete`), the
Event, the form, `outcome` (`ok`, `refused`, `forms_unavailable`) and, for
the last, `reason` (`forbidden`, `invalid`, `failed`); never who asked, an
answer or a question's text.

## Forms' reads with fields

`formsfeed.Client` asks for the mapped answers with `fields`, the
question ids each once, sorted, comma-joined (Forms: at most 20, each at
most 100 characters; core refuses more before it sends):

```http
GET {FORMS_INTERNAL_URL}/internal/v1/forms/{formId}/responses?status=accepted&…&fields=q1,q2,q3
GET {FORMS_INTERNAL_URL}/internal/v1/forms/{formId}/responses/{responseId}?fields=q1,q2,q3
```

Each item then carries `answers: {questionId: text}`, only the questions
asked for (`null` without `fields`): an answer nobody mapped (an id
number, free text, a phone number no mapping names) never leaves Forms.
Core keeps the answers in memory for the length of one answer; it never
logs them, and stores only the details it reads from them, on the guest's
Ticket ([the details](#the-details)).

## Tickets from a mapping

Who an answer names is decided in one place, `formsfeed.ResolveRespondents`,
for every way an answer comes in: the HTTP report
([form-response-tickets.md](form-response-tickets.md)), a `core.forms`
event and a pull ([form-pulls.md](form-pulls.md)). In this order:

1. a `userId`: that person;
2. else the guest Forms names (the form's identity fields): that guest;
3. else, for an accepted answer to a form an Event that lists it now maps:
   the guest built from the mapped answers;
4. else nobody: `no_respondent`, as before.

Writing the Ticket is apart (`formsfeed.RecordRespondents`), always by the
one Ticket rule (`ticket.Service.RecordFormResponse`): capacity, waiting
list, closed applying by `submittedAt`, cancelled and ended Events, a
declined answer's cancellation (`tickets.form_response_id`). A mapped guest
is written only on the Events whose mapping built it
(`FormResponse.GuestEvents`): an Event that lists the same form without a
mapping (a form shared before the one-place rule) gets no guest. The guest
is written as every forms guest is: an e-mail with a Ticket on the Event
already gets no second one, and the guest is never renamed.

How each way reads the answers:

- **HTTP report** (`formsfeed.MappedGuests.RecordReport`): the report
  carries no answers, so core reads the single answer with `fields`. A read
  that finds nothing is `gone` (a 2xx); another failure answers 5xx, and
  Forms sends the report again.
- **`core.forms` event**: an event carries no answers; the single read
  (claim check) asks for `fields` too.
- **Pull**: each page is asked with `fields`; an item's `answers` build the
  guest without a single read.

The guest from the answers (SPEC §5.3):

- e-mail: trimmed, in lower case, one plain address with a dotted domain;
- first and last name questions: trimmed, inner spaces folded, at most 100
  characters each;
- a full name question fills only what those leave empty: the last word is
  the last name, the words before it the first name.

Two decisions of Yusuf (SPEC §11, 2026-11-25, as proposed), each in one
place:

- **K3**, a full name of one word: no Ticket, counted `mapping_incomplete`
  (`splitFullName`).
- **K4**, an e-mail that is a member's account's: a guest Ticket, as every
  guest Ticket; matching it to the member is later work (`MappedGuest`).

New outcomes of the `form_response` line and
`skylab_forms_answers_total{outcome}`: `mapped_guest` (a guest Ticket
written or found through a mapping) and `mapping_incomplete` (an e-mail
that is not an address, or no first and last name; no Ticket). Neither
the line nor the metric names anybody.

With `EVENTS_FORMS_ENABLED` off none of this runs: an answer that names
nobody is `no_respondent`, as before.

## The details

The optional `phone`, `university`, `department` and `grade` questions
(Yusuf, 2026-11-26) fill the guest Ticket's columns a Walk-in fills
([walk-in.md](walk-in.md)): `guest_phone_number`, `guest_university`,
`guest_department`, `guest_grade`, for the Event's statistics and the
attendee exports. Only a **guest's** Ticket takes them, whoever names the
guest: a mapped guest, and Forms' named guest (the form's identity fields)
too. A signed-in person's answer takes none: a member's details live in
their account.

- A mapped guest's answer is read with every mapped question, as before;
  an answer Forms names a guest of is read with the details' questions
  only, and only when a mapping names any (otherwise nothing changes: no
  `fields`, or no read for the HTTP report).
- Read in `formsfeed.MappedDetails`, each detail on its own (an answer is a
  stranger's text: a detail that does not read is left out, never the
  guest):
  - the phone in the Walk-in's one form (`ticket.NormalizePhone`: `+90` and
    ten digits, or `+` and 8 to 15 digits); not a number, none;
  - the university and the department trimmed, inner spaces folded; over
    200 characters or with a control character, none;
  - the grade as one of the Walk-in's keys (`ticket.Grades`) by
    `formsfeed.GradeOf`: `prep` (hazırlık), `1` to `4` (the one year it
    names, in digits or words: "3. Sınıf", "Üçüncü"), `masters` (yüksek
    lisans, master, YL), `phd` (doktora), `graduate` (mezun); anything
    else answered, a year past 4, or more than one of them (a multiple
    choice's "1. Sınıf, 2. Sınıf"), `other`. Forms keeps a choice
    question's answer as the option's text, so a choice question reads.
    The answer itself is not kept (Yusuf, 2026-11-26): the column holds
    the key alone; nothing reads a free text, and it would be a stranger's.
- A new guest Ticket takes the details; one already there takes only those
  it has none of (as its phone number always did): a value it has is never
  replaced. An Unticketed respondent's row keeps them until its Ticket is
  sent ([form-respondents.md](form-respondents.md#data)).
- Panel: the four boxes are optional ("Telefon", "Üniversite", "Bölüm",
  "Sınıf"); a choice question may be chosen for them. The Ticket's detail
  shows them to who may see the Event's Tickets, the grade in Turkish.

## Filling in earlier guests

Answers that came before a mapping named the details left the guests'
Tickets without them. A one-time command fills them, inside the core
container (it reads the server's environment):

```
core-backend backfill-form-guest-details -event <id> [-apply]
```

- It reads the Event's saved mappings that name a detail (of forms the
  Event lists); none: `no_details_mapping: 1`, exit 3 (the organizer
  saves the questions first, panel "Soru eşleme").
- For each such form it reads every answer that is not archived from Forms
  (the list, `fields` the details' questions only, a page of 100 at a time)
  and puts each answer's details (`MappedDetails`) on the guest Ticket of
  the Event that answer wrote (`tickets.form_response_id`; ticket type
  GUEST, one linked to the guest's account since too; not a cancelled
  Ticket, nor one whose identity retention cleared) and on its Unticketed
  respondent's guest row, only where the row has none: a run again finds
  nothing to do. Never a signed-in person's Ticket.
- Without `-apply` it only counts. It prints counts only (`tickets`,
  `tickets_fill`, `respondents_fill`, `fill_<detail>`, `kept_<detail>` for
  a Ticket keeping another value of its own, `dropped_<detail>` for an
  answer that did not read, `grade_<key>`), never a person, an answer or a
  value. Exit 0; 1 a failure (Forms, the database, no such Event); 2 its
  usage or environment.
- The write is one short batch of row updates a page; the door neither
  waits for it nor reads what it writes (no check-in locks a Ticket's row
  for update). ARTLAB's run waits for the Event's end all the same
  (2026-11-26).

## Table

Migration `20261127100000`: `event_form_mappings` (`event_id`, `form_id`,
`first_name_question`, `last_name_question`, `full_name_question`,
`email_question`, `updated_by`, `updated_at`), key (`event_id`, `form_id`),
the rule as checks, an index on `form_id`. The Event's deletion takes its
rows. A row whose form the Event no longer lists is not read, nor
answered (`404 form_not_listed`); linking the form again reads it again.
Ids and times only; `updated_by` is the subject who saved it last.

Migration `20261128010000` adds the details' questions
(`phone_question`, `university_question`, `department_question`,
`grade_question`, nullable, the ids' check
`event_form_mappings_details_check`) and a respondent row's details
([form-respondents.md](form-respondents.md#data)). Its down keeps the
columns and their values: the release before neither reads nor writes
them.

## Against the standard

Matches: a field mapping kept on the consumer's side, as Zapier's and
Microsoft Power Automate's form triggers keep the fields a step reads;
asking the producer for named fields only (Microsoft Graph `$select`,
Google APIs' `fields`), data minimisation by the request. Deviations: the
ids are checked against the form when saved, not when read later (a
question deleted in Forms since then makes that answer's mapping
incomplete); a mapping is not deleted when the form leaves the Event, it is
only not read (no hook in the Event writes; linking the form back keeps it).

## Rolling back

The code before neither reads nor writes the table, and its routes answer
`404`. The migration's down keeps the table and its rows (an organizer's
mapping is not lost to a rollback); `DROP TABLE event_form_mappings` by
hand removes it for good.
