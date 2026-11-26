package event

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// TicketNoteMax bounds an Event's ticket note, in characters (the
// database's check says the same): a short announcement, a link or two.
const TicketNoteMax = 500

// ErrTicketNoteInvalid is a ticket note that cannot be stored: over
// TicketNoteMax characters, or holding a character badNoteRune refuses
// (docs/ticket-note.md).
var ErrTicketNoteInvalid = fmt.Errorf("event: invalid ticket note: %w", ErrInvalid)

// normalizeTicketNote writes e's ticket note the way it is stored: trimmed,
// line breaks as \n, as the attendee note. Nil (a write that leaves it)
// stays nil.
func normalizeTicketNote(e Event) (Event, error) {
	if e.TicketNote == nil {
		return e, nil
	}
	note := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(*e.TicketNote, "\r\n", "\n"), "\r", "\n"))
	if utf8.RuneCountInString(note) > TicketNoteMax || !utf8.ValidString(note) || strings.IndexFunc(note, badNoteRune) >= 0 {
		return Event{}, ErrTicketNoteInvalid
	}
	e.TicketNote = &note
	return e, nil
}

// NotePart is a piece of a note as a mail shows it: text, and its address
// when the piece is a link (the template writes it as one).
type NotePart struct {
	Text string `json:"Text"`
	URL  string `json:"Url"`
}

// httpsAddress is an https address in plain text, up to the next space
// (a Unicode one too, U+00A0 say) or quote; trailing punctuation is not
// part of it. An http address stays text: a mail links https alone.
var httpsAddress = regexp.MustCompile(`https://[^\s\p{Z}<>"']+`)

// TicketNoteParts cuts e's ticket note at its https addresses for the
// ticket mails (docs/ticket-note.md): the note and its parts, "" and []
// for none.
func TicketNoteParts(e Event) (string, []NotePart) {
	note := ""
	if e.TicketNote != nil {
		note = *e.TicketNote
	}
	parts := []NotePart{}
	rest := note
	for {
		loc := httpsAddress.FindStringIndex(rest)
		if loc == nil {
			break
		}
		address := strings.TrimRight(rest[loc[0]:loc[1]], ".,;:!?)]}")
		end := loc[0] + len(address)
		if address == "https://" {
			// Nothing after the scheme: text, not a link.
			parts = append(parts, NotePart{Text: rest[:end]})
			rest = rest[end:]
			continue
		}
		if loc[0] > 0 {
			parts = append(parts, NotePart{Text: rest[:loc[0]]})
		}
		parts = append(parts, NotePart{Text: address, URL: address})
		rest = rest[end:]
	}
	if rest != "" {
		parts = append(parts, NotePart{Text: rest})
	}
	return note, parts
}
