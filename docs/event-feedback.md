# Event feedback forms

Etkinlik geri bildirim formu (spec `.scratch/etkinlik-geri-bildirim-formu/SPEC.md` in the hub; Yusuf's decisions of 2026-10-20). An Event names its feedback form apart from its application and extra forms: **an answer to a feedback form writes no Ticket.**

This page is ticket 02: the Event's fields, the one-place rule, the short link and the forms report. Session forms (ticket 03), the invite record and who answered (04), the mails (06) and the reminder (07) come after it.

## Fields

On `GET /v1/events/{id}` and every Event JSON; read as the application form is (anyone who sees the Event sees them: whoever has the link may answer).

| Field | Meaning |
|---|---|
| `feedbackMode` | `event` (default): one form for the Event. `per_session`: a form per Session, the Event's form optional for the Event as a whole. `event_with_sessions`: one Event form that asks about each Session itself (the form's own structure; core keeps none of it) |
| `feedbackFormUrl` | the form's address; the form is its first UUID path segment, as for `formUrl`. Left out when none |
| `feedbackFormAlias` | the short link alias the Event gives it (the panel makes it, as for `formAlias`). Left out when none |
| `feedbackAutoSend` | mail the form when the Event ends (ticket 06; the panel's "Bitince gönder" box). Default `true` |

`POST /v1/events` takes all four (no `feedbackMode` is `event`, no `feedbackAutoSend` is `true`). `PUT`/`PATCH` keep what the body leaves out ([event-updates.md](event-updates.md)): `feedbackFormUrl` and `feedbackFormAlias` are cleared with `null` or `""`; `feedbackMode` and `feedbackAutoSend` may not be `null`; an unknown mode is `400`.

A copy ([event-copy.md](event-copy.md)) takes `feedbackMode`, not the form (a form is in one place only): its `feedbackAutoSend` is `true`, and the panel offers "Geçen seferin formundan yeni form".

## One place per feedback form

A feedback form is in one place only. Creating or updating an Event (an update that sends a form, an alias or the name) is `409` with `code: "form_in_use"` when:

- its feedback form is one of its own application or extra forms;
- its feedback form is an application, extra or feedback form of another current Event (a draft too);
- one of its application or extra forms is another current Event's feedback form.

Application and extra forms may still be shared between Events, as before. An archived Event holds no form: its forms are free once it is archived, and restoring it while one of them is in another place is `409 form_in_use`. The check reads the current Events when the Event is written; two writes racing for the same form are not serialized (an organizer's slip, which the panel's link step also catches).

## Short link

The feedback form's link is the Event's to manage, as its application forms' links are ([form-short-links.md](form-short-links.md)): synced with `feedbackFormAlias`, labelled "<Event> · Geri bildirim", released when the form is cleared or the Event archived.

## Forms report

`POST /v1/forms/{formId}/responses` for a current Event's feedback form answers `204` and writes nothing, whatever the status (`pending`, `accepted`, `declined`) and whoever answered (a guest needs no name: a feedback form asks for none). The log line says `outcome: feedback`. Forms sees a `2xx` and does not send the report again. Drafts, cancelled Events and Events closed to applications behave the same ([form-response-tickets.md](form-response-tickets.md)).
