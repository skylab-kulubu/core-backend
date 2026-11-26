package ticketpage

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// noteLine is a Session's description as the programme prints it under the
// title: its first line that has text, its spaces collapsed, cut at a word
// to at most limit characters with "…".
func noteLine(description string, limit int) string {
	line := ""
	for raw := range strings.SplitSeq(description, "\n") {
		if line = strings.Join(strings.Fields(raw), " "); line != "" {
			break
		}
	}
	if limit <= 0 || utf8.RuneCountInString(line) <= limit {
		return line
	}
	runes := []rune(line)[:limit-1]
	cut := string(runes)
	if at := strings.LastIndexByte(cut, ' '); at > len(cut)/2 {
		cut = cut[:at]
	}
	return strings.TrimRight(cut, " ,;·-–") + "…"
}

// isEmoji reports whether r is drawn as an emoji (a pictograph, or a mark
// that only shapes one: a variation selector, the zero-width joiner, a
// keycap, a tag): what neither the ticket's typefaces nor the renderer's
// draw, which would print as an empty box.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, transport, flags' letters, skin tones
		r >= 0x2600 && r <= 0x27BF, // miscellaneous symbols, dingbats
		r >= 0x2B05 && r <= 0x2B07, r == 0x2B1B, r == 0x2B1C, r == 0x2B50, r == 0x2B55,
		r == 0x231A, r == 0x231B, r == 0x2328, r == 0x23CF, r >= 0x23E9 && r <= 0x23F3, r >= 0x23F8 && r <= 0x23FA,
		r == 0x3030, r == 0x303D, r == 0x3297, r == 0x3299,
		r == 0xFE0E, r == 0xFE0F, r == 0x200D, r == 0x20E3,
		r >= 0xE0020 && r <= 0xE007F:
		return true
	}
	return false
}

// stripEmoji is s without its emoji, the spaces they leave collapsed and
// each line trimmed; s as it is when it has none.
func stripEmoji(s string) string {
	if !strings.ContainsFunc(s, isEmoji) {
		return s
	}
	lines := strings.Split(strings.Map(func(r rune) rune {
		if isEmoji(r) {
			return -1
		}
		return r
	}, s), "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || unicode.Is(unicode.Zs, r) }), " ")
	}
	return strings.Join(lines, "\n")
}

// withoutEmoji is d with no emoji in the text it prints (docs/ticket-page.md):
// the PDF's typefaces have none, and the renderer would draw a box. The
// ticket page and the mails keep the text as it is.
func withoutEmoji(d Document) Document {
	for _, field := range []*string{&d.EventName, &d.Venue, &d.VenueAddress, &d.OwnerTeam, &d.CoOrganizers, &d.HolderName, &d.Description, &d.TicketNote} {
		*field = stripEmoji(*field)
	}
	if len(d.Days) == 0 {
		return d
	}
	days := make([]ProgramDay, len(d.Days))
	for i, day := range d.Days {
		days[i] = ProgramDay{Date: day.Date, Sessions: make([]ProgramSession, len(day.Sessions))}
		for j, sess := range day.Sessions {
			sess.Title, sess.Speaker, sess.Note = stripEmoji(sess.Title), stripEmoji(sess.Speaker), stripEmoji(sess.Note)
			days[i].Sessions[j] = sess
		}
	}
	d.Days = days
	return d
}
