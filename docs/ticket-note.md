# Ticket note ("Bilet duyurusu")

`ticketNote` (Yusuf, 2026-11-26) is the organizer's short announcement for
the Ticket holders: the event's WhatsApp group, say. Plain text, at most
500 characters (`events.ticket_note`, a database check too), trimmed, line
breaks kept; a longer text or a character the attendee note refuses (a
control character other than a line break or a tab, a direction override
or isolate, a zero-width character) is `400 event_ticket_note_invalid`.

- `POST /v1/events` and `PUT /v1/events/{id}` take it (left out keeps it);
  `PATCH` writes it, `null` or `""` clears it. A copy does not take it (the
  next event has its own group).
- Who reads it in an answer is who reads the attendee note
  (docs/attendance-mail.md): who may edit the Event, or read its Tickets.
  Every other answer leaves the key out (`Event.TicketNote` is `json:"-"`;
  `ProjectFor` copies it).
- It is not the attendee note: that one is the door's (Wi-Fi, the room) and
  goes in the welcome mail alone; this one goes to everyone with a Ticket,
  before the day.

## In the mails

`event.ticket-created`, `event.waitlist-promoted` and `event.reminder`
carry two more variables, always there:

| Variable | Value |
|---|---|
| `TicketNote` | the note as stored, `""` for none |
| `TicketNoteParts` | `[{Text, Url}]`: the note cut at its `https://` addresses (trailing punctuation left out), `Url` empty for plain text; `[]` for none |

Only `https://` addresses become links: an `http://` address, or anything
else, stays text. The template (skynotify-frontend `TicketNote` in
`emails/theme.tsx`) shows the section "Duyuru" only when `TicketNote` is
set; SkyNotify renders with Go's `html/template`, which escapes the text
and the address. A template seeded before the note ignores the two
variables, and the note's template with a core of before shows nothing.

The waiting list's and the cancellation's mails do not carry it. The
ticket page and the PDF ticket do not show it (yet).
