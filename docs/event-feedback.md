# Event feedback forms

Etkinlik geri bildirim formu (spec `.scratch/etkinlik-geri-bildirim-formu/SPEC.md` in the hub; Yusuf's decisions of 2026-10-20). An Event names its feedback form apart from its application and extra forms: **an answer to a feedback form writes no Ticket.**

This page is tickets 02 and 03: the Event's and the Sessions' fields, the one-place rule, the short links and the forms report. The invite record and who answered (04), the mails (06) and the reminder (07) come after them.

## Fields

On `GET /v1/events/{id}` and every Event JSON; read as the application form is (anyone who sees the Event sees them: whoever has the link may answer).

| Field | Meaning |
|---|---|
| `feedbackMode` | `event` (default): one form for the Event. `per_session`: a form per Session, the Event's form optional for the Event as a whole. `event_with_sessions`: one Event form that asks about each Session itself (the form's own structure; core keeps none of it) |
| `feedbackFormUrl` | the form's address; the form is its first UUID path segment, as for `formUrl`. Left out when none |
| `feedbackFormAlias` | the short link alias the Event gives it (the panel makes it, as for `formAlias`). Left out when none |
| `feedbackAutoSend` | mail the form when the Event ends (ticket 06; the panel's "Bitince gönder" box). Default `true` |

`POST /v1/events` takes all four (no `feedbackMode` is `event`, no `feedbackAutoSend` is `true`). `PUT`/`PATCH` keep what the body leaves out ([event-updates.md](event-updates.md)): `feedbackFormUrl` and `feedbackFormAlias` are cleared with `null` or `""`; `feedbackMode` and `feedbackAutoSend` may not be `null`; an unknown mode is `400`.

A copy ([event-copy.md](event-copy.md)) takes `feedbackMode` and `feedbackAutoSend` (the organizer's choices), not the form (a form is in one place only): the panel offers "Geçen seferin formundan yeni form".

### A Session's form

In the `per_session` mode each Session may have its own form, on the Session JSON (`GET /v1/sessions/{id}`, the day's list):

| Field | Meaning |
|---|---|
| `feedbackFormUrl`, `feedbackFormAlias` | the Session's form and its short link alias; left out when none |
| `feedbackAutoSend` | mail it when the Session ends; default `true` |

`POST /v1/sessions` takes them (no `feedbackAutoSend` is `true`). `PUT /v1/sessions/{id}` stays a whole-body update for the Session's other fields, but these three are kept when the body leaves them out (sky-app and the panel before this send none), as `cancelled` is: `null` or `""` clears the form or the alias, `feedbackAutoSend: null` is `400`. The fields stay when the mode is not `per_session` (no mail goes for them then; the panel shows them in that mode only). A copied Session starts without a form; its `feedbackAutoSend` comes along.

## One place per feedback form

A feedback form is in one place only. Creating or updating an Event (an update that sends a form, an alias or the name) is `409` with `code: "form_in_use"` when:

- its feedback form is one of its own application or extra forms;
- its feedback form is an application, extra or feedback form of another current Event (a draft too), or a Session's feedback form;
- one of its application or extra forms is another current Event's feedback form or a Session's feedback form (its own Sessions' too).

Creating or updating a Session with a form is `409 form_in_use` when the form is any form of a current Event (its own included) or another current Session's feedback form.

Application and extra forms may still be shared between Events, as before. An archived Event, day or Session holds no form: its forms are free once it is archived, and restoring it while one of them is in another place is `409 form_in_use`. A write of an Event or a Session applies the rule again inside its own transaction, after a transaction-scoped advisory lock on each of its forms (`pg_advisory_xact_lock`, in a fixed order): two writes naming the same form are decided one after the other, and the second sees the first. Restoring an archived Event, day or Session checks without that lock (rare, by hand).

## Short link

The feedback forms' links are the Event's to manage, as its application forms' links are ([form-short-links.md](form-short-links.md)): synced with `feedbackFormAlias`, labelled "<Event> · Geri bildirim", and for a Session "<Event> · <Session> · Geri bildirim"; released when the form is cleared or the Event, the day or the Session is archived. Every sync of an Event carries its Sessions' forms, so saving the Event keeps their links; a Session's change syncs its Event's links (only when its form, alias or, with a form, its title changes).

## Forms report

`POST /v1/forms/{formId}/responses` for a current Event's or Session's feedback form answers `204` and writes nothing, whatever the status (`pending`, `accepted`, `declined`) and whoever answered (a guest needs no name: a feedback form asks for none). The log line says `outcome: feedback`. Forms sees a `2xx` and does not send the report again. Drafts, cancelled Events and Events closed to applications behave the same ([form-response-tickets.md](form-response-tickets.md)).
