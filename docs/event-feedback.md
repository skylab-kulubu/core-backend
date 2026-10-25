# Event feedback forms

Etkinlik geri bildirim formu (spec `.scratch/etkinlik-geri-bildirim-formu/SPEC.md` in the hub; Yusuf's decisions of 2026-10-20). An Event names its feedback form apart from its application and extra forms: **an answer to a feedback form writes no Ticket.**

This page is tickets 02 to 04 and 06: the Event's and the Sessions' fields, the one-place rule, the short links, the forms report, the invites and who answered, and the mails. The reminder (07) comes after them.

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

`POST /v1/forms/{formId}/responses` for a current Event's or Session's feedback form answers `204` and writes no Ticket, whatever the status (`pending`, `accepted`, `declined`) and whoever answered (a guest needs no name: a feedback form asks for none). The log line says `outcome: feedback`. Forms sees a `2xx` and does not send the report again. Drafts, cancelled Events and Events closed to applications behave the same ([form-response-tickets.md](form-response-tickets.md)). A `pending` or `accepted` answer marks its invite answered (below); a `declined` one changes nothing and does not take back an earlier mark.

## Invites and who answered

Each mail of a feedback form (ticket 06) writes one `feedback_invites` row per Ticket and target (the Event, or one Session; a second for the same pair is refused): the form, a code of its own (128 random bits, base64url, 22 characters), `sent_at`, and later `reminded_at`, `answered_at` and `reminder_cancelled_at` (the form closed, ticket 07; set once). The mail's link is the form's own address with the code alone, `https://forms.yildizskylab.com/<formId>?davet=<code>` (`feedback.Link`): not the short link, whose clicks are recorded with their query, and nothing else, not the Sessions attended (decision S1).

The code is stored plain: the reminder sends the same link again, and all a code can do is mark its invite answered (someone the mail was forwarded to can at most keep the reminder from going).

A forms report for a feedback form marks, in this order:

1. the invite whose code the report's `invite` carries, when it is this form's;
2. otherwise, for an answer given signed in (`userId`), this form's invites of that person's Tickets (no Forms change needed);
3. otherwise nothing.

An unknown code, another form's, or one longer than 64 characters is ignored. A guest is matched by the code alone: until Forms carries `invite` in its report (Fatih, ticket 11) a guest never counts as answered and gets the reminder, which is why it says "henüz doldurmadıysan". A report sent again changes nothing.

**Counts.** `GET /v1/events/{id}/feedback` answers `{"event": {"sent": 12, "answered": 5}, "sessions": [{"sessionId": "…", "sent": 4, "answered": 1}]}`: how many invites each target's form went out in and how many were answered. Who may read the Event in full may (its editors, DK); a member `403`, a draft the caller may not see `404`. The counts live in `feedback_invite_counts`, kept by a trigger on the invites, and stay when the invites go.

**Retention and erasure.** An invite is deleted 90 days after it was sent (retention rule `feedback_invites_age`, [retention-sweep.md](retention-sweep.md)); a feedback mail of the queue, with the targets it carried, 90 days after it was done with (`feedback_mails_age`). Account erasure deletes the invites of the person's Tickets and of their addresses' guest Tickets ([account-lifecycle.md](account-lifecycle.md)); a Ticket is never deleted, so its invite does not go with it. The counts stay in both cases: they name nobody.

## Mails (ticket 06)

**Which targets, when.** A target is the Event's form (any mode) or, in the `per_session` mode, a Session's form. The schedule (`internal/feedbackmail`, every minute, one replica at a time under an advisory lock) finds the targets that ended `feedback.SendDelay` (2 hours) ago: the Event's end (`endDate`, else a day after `startDate`), a Session's `endTime`, else its day's end, else the Event's. Only with a form and `feedbackAutoSend` on, the Event published and current, a Session not cancelled or archived, and not sent before. A target with no end is never sent by itself. A target that ended more than `feedback.AutoWindow` (7 days) ago is not sent by itself either, so a form added to an old Event does not mail its people out of the blue; "Şimdi gönder" (below) still sends it (Yusuf, 2026-10-20).

**Whom.** The Tickets that checked in: to the Session, or, for the Event's form, to any of its Sessions; guests too. The address is read when the mail goes, as for the Event status mails: a member whose account is erased, being erased or blocked, or a guest without an address, gets none.

**One mail a person a day (S2).** A target taken by the schedule is recorded once (`feedback_mail_targets`) and put into each such Ticket's mail of its EventDay (the Session's day; the Event's form joins the Event's last day). That mail goes `SendDelay` (plus a 5-minute grace, so that targets of the same minute join it) after the last Session the person checked in to that day ends. One mail, all that day's forms: `core.feedback-request` with `FirstName` (may be empty), `EventName` and `Items`, one `{Title, URL}` per form; `Title` the Session's title or the Event's name, `URL` the form's own address with the person's invite code (`feedback.Link`), never a short link. A person who left after the morning gets the morning's Session forms after their last Session, and the Event's form in a second mail once the Event has ended (Yusuf, 2026-10-20: two mails then, never the Event's form before the Event ends).

**One address, one mail a day** (decision of 2026-10-20). The Tickets of one Event held under one address (a person with a member and a guest Ticket; the address as the mail reads it, trimmed and lower-cased) get one mail a day: a scheduled mail waits until the last Session any of them checked in to that day ends plus `SendDelay` (`feedback.PostgresStore.DayDue`), and then takes the open mails of the same Event, day and kind to the same address with it. Each Ticket keeps its own invites, so `Items` may hold two links to the same form. "Şimdi gönder" mails to one address are joined the same way. Two passes on two replicas taking two such mails in the same second may still send two.

**"Şimdi gönder".** `POST /v1/events/{id}/feedback/send` (the Event's form) and `POST /v1/sessions/{id}/feedback/send` (a Session's): whoever may edit the Event. `202 {"sent": true, "recipients": n}`: a mail of its own to each Ticket that checked in, at once, not waiting for the day's others; the target counts as sent and the schedule never sends it again. `200 {"sent": false}` when it was sent already. `409 feedback_form_missing` without a form; `409 feedback_not_sendable` for a draft, a cancelled or archived Event, a cancelled or archived Session. The box (`feedbackAutoSend`) does not stop it.

**When it goes.** Before each mail the worker reads its targets again: one whose Event is no longer published (cancelled, back to draft) or archived, whose Session was cancelled or archived, whose form changed, or (the schedule's mail) whose box was turned off or whose Session is no longer in the `per_session` mode, is left out; a mail with nothing left is dropped. The invites are written as the mail is built, one per Ticket and target; a mail tried again (SkyMail down) sends the same links, nothing twice. A refusal by SkyMail (`400`, `422`) is done with; any other failure is tried again with a doubling wait; a mail not sent 72 hours after it was queued (`feedbackmail.GiveUpAfter`) is dropped, tried or not. With `SKYMAIL_FEEDBACK_REQUEST_TEMPLATE_KEY` empty nothing is planned: no target is used up and no mail piles up; set again, the schedule takes the targets still within `AutoWindow`, as after SkyMail was down, and a mail queued before the key went off expires as above. Without SkyMail the worker is off and nothing is planned.


## Reminder (ticket 07)

**Once, three days later (K4).** One reminder a Ticket a day (ADR 0069 decision 9: the reminder is one mail too). `feedback.ReminderDelay` (3 days) after the Ticket's last feedback mail of the day went (SkyMail took it; a mail it refused, or one dropped, gets none), the same worker queues one reminder with the targets of all that day's mails whose invite is not answered, reminded or cancelled. A person who got the morning's Session forms and the Event's form in a second mail gets one reminder, three days after the second; while a mail of the day is still to go, the day waits for it. It goes as `core.feedback-reminder` with the same variables and the **same links** (the same invite codes), joined into one mail an address as the first one. Each mail is planned once (`feedback_mails.reminder_planned_at`), each invite once (`reminded_at`). A reminder that fell due more than `feedback.ReminderWindow` (7 days) ago (core was off meanwhile) is not sent. A guest is reminded until Forms reports the invite code with their answer (ticket 11): until then a guest never counts as having answered, so the text says "if you have not filled it in yet".

**When it goes.** As the schedule's mail, whoever sent the form: the worker reads the targets again, and one whose Event is no longer published or is archived, whose Session was cancelled or archived, whose form changed, whose box (`feedbackAutoSend`) was turned off, or whose Session is no longer in the `per_session` mode, is left out. "Şimdi gönder" with the box off sends the form but no reminder. A reminder with nothing left is not sent. With `SKYMAIL_FEEDBACK_REMINDER_TEMPLATE_KEY` empty no reminder is planned (the requests still go); set again, the reminders still within `ReminderWindow` are planned.

**The form closed: cancelled for good (S3).** Once a target's form closes, the reminders still to go of its invites are cancelled (`feedback_invites.reminder_cancelled_at`); opening the form again does not bring them back.

- The panel: `POST /v1/events/{id}/feedback/close` (the Event's form) and `POST /v1/sessions/{id}/feedback/close` (a Session's), whoever may edit the Event, when its "Form açık/kapalı" switch closes the form in Forms. `200 {"cancelled": n}`. It cancels the invites written so far.
- Forms: its word comes as the `forms.form.updated` event (ADR 0071; `status` `open`/`closed`/`deleted`, `closesAt`), which core's `core.forms` consumer (RabbitMQ ticket R13, not built yet) hands to `feedback.PostgresStore.ReportFormState`. Forms makes no call to core for it (Yusuf, 2026-10-20, forms-backend#38: the `PUT /v1/forms/{formId}/state` first proposed is dropped). Core keeps the closing time of a feedback form only (`feedback_form_closings`: a current Event's or Session's feedback form, matched by its id in the form address, or a form invites went out for); any other form's word changes nothing. From the closing time on, the form's invites' reminders are cancelled: at once when it has come, else by the worker's first pass after it, and invites written later are not reminded either. An open form with no closing time deletes the row; a closed form's closing time is now, and an earlier one is kept. Until R13, only the panel's switch cancels.
