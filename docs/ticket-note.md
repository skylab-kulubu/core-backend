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
carry four more variables, always there:

| Variable | Value |
|---|---|
| `TicketNote` | the note as stored, `""` for none |
| `TicketNoteParts` | `[{Text, Url}]`: the note cut at its `https://` addresses (trailing punctuation left out), `Url` empty for plain text; `[]` for none |
| `TicketNoteLink` | the address the note's button opens: its first WhatsApp address (`chat.whatsapp.com`, `whatsapp.com`, `wa.me`), else its first `https://` one; `""` for none |
| `TicketNoteWhatsApp` | `"1"` when `TicketNoteLink` is a WhatsApp address, `""` otherwise |

### The button

Kağan and Yusuf (2026-11-26) asked for the WhatsApp group to stand out: the
template draws the note in a box and, when `TicketNoteLink` is set, a
table-based button under it, "WhatsApp grubuna katıl" for a WhatsApp
address and "Bağlantıyı aç" for any other. Without an address there is no
button. A core of before sends neither variable: the box shows the note,
no button.

Only `https://` addresses become links: an `http://` address, or anything
else, stays text. The template (skynotify-frontend `TicketNote` in
`emails/theme.tsx`) shows the section "Duyuru" only when `TicketNote` is
set; SkyNotify renders with Go's `html/template`, which escapes the text
and the address. A template seeded before the note ignores the two
variables, and the note's template with a core of before shows nothing.

The waiting list's and the cancellation's mails do not carry it. The
ticket page does not show it (yet).

## In the PDF ticket

The PDF ticket (ticket-page.md) shows it in a section "Duyuru" beside
"Etkinlik yeri", last on the page (beside, not above: the ticket keeps to
one page): the note's text, its https addresses links, and for the
button's address (`TicketNoteLink`'s rule) a QR on white with "WhatsApp
grubuna katıl" ("Bağlantıyı aç" for another address) under it, underlined
in the theme's accent; the QR and the line are links. No address, no QR;
no note, no section. Its emoji are left out, as from every text the PDF
prints.
