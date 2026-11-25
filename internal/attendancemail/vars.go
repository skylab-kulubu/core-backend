package attendancemail

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
)

// title is a Session as the templates range over it.
type title struct {
	Title string `json:"Title"`
}

// slot is a programme line: "10:00–10:45", "Açılış".
type slot struct {
	Time  string `json:"Time"`
	Title string `json:"Title"`
}

// counts is the certificate rule as it stands for one Ticket
// (docs/certificate-rule.md): N live counted Sessions, x of them attended,
// the threshold M, and the required ones not attended, split into those
// still to come and those over.
type counts struct {
	hasRule                         bool
	attended, counted, min          int
	missingRequired, missedRequired []title
	required                        []string
}

func (s *Service) counts(ev event.Event, p Progress) counts {
	var c counts
	c.missingRequired, c.missedRequired = []title{}, []title{}
	for _, sess := range p.Sessions {
		if sess.Counted {
			c.counted++
			if p.Attended[sess.ID] {
				c.attended++
			}
		}
	}
	c.hasRule = ev.AttendanceRule != event.RuleNone && ev.AttendanceRule != "" && c.counted > 0
	if !c.hasRule {
		return counts{missingRequired: []title{}, missedRequired: []title{}}
	}
	c.min = event.RuleMin(ev, c.counted)
	now := s.now()
	for _, sess := range p.Sessions {
		if !sess.Required {
			continue
		}
		c.required = append(c.required, sess.Title)
		if p.Attended[sess.ID] {
			continue
		}
		if sess.Ends != nil && !sess.Ends.After(now) {
			c.missedRequired = append(c.missedRequired, title{sess.Title})
		} else {
			c.missingRequired = append(c.missingRequired, title{sess.Title})
		}
	}
	return c
}

// vars are the type's variables (skymail-frontend
// emails/event-attendance-welcome.tsx and event-attendance-recorded.tsx):
// every key always there, empty or 0 without a value (SkyMail renders with
// missingkey=error). A welcome carries the receipts that went along in
// Recorded.
func (s *Service) vars(kind string, ev event.Event, firstName string, rows []Mail, p Progress) map[string]any {
	c := s.counts(ev, p)
	sessions, notCounted := []title{}, []title{}
	counted := map[uuid.UUID]bool{}
	for _, sess := range p.Sessions {
		counted[sess.ID] = sess.Counted
	}
	date := ""
	for _, m := range rows {
		if m.Kind == KindWelcome || date == "" {
			date = m.EntryDate
		}
		if m.SessionID == nil {
			continue
		}
		name := p.Titles[*m.SessionID]
		sessions = append(sessions, title{name})
		if c.hasRule && !counted[*m.SessionID] {
			notCounted = append(notCounted, title{name})
		}
	}
	vars := map[string]any{
		"FirstName": firstName,
		"EventName": ev.Name,
		"EventId":   ev.ID.String(),
		"OwnerTeam": organizer.Name(ev.OwnerTeam),
		// The Co-organizer teams shown outside the panel (docs/co-organizers.md).
		"CoOrganizers":    organizer.CoNames(event.PublicCoOrganizerTeams(ev)),
		"Attended":        c.attended,
		"CountedTotal":    c.counted,
		"MinRequired":     c.min,
		"MissingRequired": c.missingRequired,
		"MissedRequired":  c.missedRequired,
	}
	if kind == KindRecorded {
		vars["Sessions"] = sessions
		vars["HasRule"] = c.hasRule
		vars["NotCounted"] = notCounted
		return vars
	}
	label, program := s.day(date, p)
	vars["DayLabel"] = label
	vars["Program"] = program
	// The organizer's note (Wi-Fi, the room): empty leaves the section out
	// of the mail. AttendeeNoteParts is the same text cut at its web
	// addresses, for the template to make them links.
	note := ""
	if ev.AttendeeNote != nil {
		note = *ev.AttendeeNote
	}
	vars["AttendeeNote"] = note
	vars["AttendeeNoteParts"] = noteParts(note)
	vars["CertificateLine"] = certificateLine(c)
	// An Event with no live Session counts the entry itself for the
	// certificate (docs/certificate-rule.md, "An Event with no Session").
	if len(p.Sessions) == 0 && ev.AttendanceRule != "" && ev.AttendanceRule != event.RuleNone {
		vars["CertificateLine"] = SessionlessCertificateLine
	}
	vars["Recorded"] = sessions
	return vars
}

// SessionlessCertificateLine is the welcome's certificate line on an Event
// with no live Session and a rule: the entry is the attendance.
const SessionlessCertificateLine = "Girişin sertifika için sayıldı."

// certificateLine is the welcome's one line about the rule: "Sertifika için
// 16 oturumdan en az 12 tanesine katılmalısın. Zorunlu: Açılış, Final."
// Empty without a rule.
func certificateLine(c counts) string {
	if !c.hasRule {
		return ""
	}
	line := "Sertifika için " + strconv.Itoa(c.counted) + " oturumdan en az " + strconv.Itoa(c.min) + " tanesine katılmalısın."
	if len(c.required) > 0 {
		line += " Zorunlu: " + strings.Join(c.required, ", ") + "."
	}
	return line
}

// day is the welcome's day label and programme for date (YYYY-MM-DD): the
// Event's current day that starts on it, its live Sessions in order. On an
// Event of several days the label says which ("2. gün · 13 Aralık
// Cumartesi"); on one of a single day, or a date no day starts on, only the
// date. An Event of one day takes that day's programme whatever its date.
func (s *Service) day(date string, p Progress) (string, []slot) {
	program := []slot{}
	on, err := time.ParseInLocation("2006-01-02", date, s.location)
	if err != nil {
		return "", program
	}
	label := ticketpage.DayLine(on.Add(12 * time.Hour))
	var dayID uuid.UUID
	index := -1
	for i, d := range p.Days {
		if d.Start != nil && d.Start.In(s.location).Format("2006-01-02") == date {
			dayID, index = d.ID, i
			break
		}
	}
	if index < 0 && len(p.Days) == 1 {
		dayID, index = p.Days[0].ID, 0
	}
	if index < 0 {
		return label, program
	}
	if len(p.Days) > 1 {
		label = strconv.Itoa(index+1) + ". gün · " + label
	}
	for _, sess := range p.Sessions {
		if sess.DayID != dayID {
			continue
		}
		program = append(program, slot{Time: s.span(sess.Start, sess.End), Title: sess.Title})
	}
	return label, program
}

// span is a Session's hours: "10:00–10:45", "10:00" without an end, empty
// without a start.
func (s *Service) span(start, end *time.Time) string {
	if start == nil {
		return ""
	}
	out := start.In(s.location).Format("15:04")
	if end != nil && end.After(*start) {
		out += "–" + end.In(s.location).Format("15:04")
	}
	return out
}

// notePart is a piece of the attendee note: text, and its address when the
// piece is a web address (the template writes it as a link).
type notePart struct {
	Text string `json:"Text"`
	URL  string `json:"Url"`
}

// webAddress is an http(s) address in plain text, up to the next space or
// quote; trailing punctuation is not part of it.
var webAddress = regexp.MustCompile(`https?://[^\s<>"']+`)

// noteParts cuts note at its web addresses: [] for an empty note.
func noteParts(note string) []notePart {
	parts := []notePart{}
	rest := note
	for {
		loc := webAddress.FindStringIndex(rest)
		if loc == nil {
			break
		}
		address := strings.TrimRight(rest[loc[0]:loc[1]], ".,;:!?)]}")
		end := loc[0] + len(address)
		if loc[0] > 0 {
			parts = append(parts, notePart{Text: rest[:loc[0]]})
		}
		parts = append(parts, notePart{Text: address, URL: address})
		rest = rest[end:]
	}
	if rest != "" {
		parts = append(parts, notePart{Text: rest})
	}
	return parts
}
