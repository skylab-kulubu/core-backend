package attendeeexport

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// The reports' sheet names, which name their files too.
var reportNames = map[string]string{
	ticket.ExportAttendees:    "Katılımcılar",
	ticket.ExportSession:      "Oturum katılımı",
	ticket.ExportMatrix:       "Katılım matrisi",
	ticket.ExportEntries:      "Alana giriş",
	ticket.ExportCertificates: "Sertifikalar",
}

// fileSlugs are the reports' names in a file name.
var fileSlugs = map[string]string{
	ticket.ExportAttendees:    "katilimcilar",
	ticket.ExportSession:      "oturum-katilimi",
	ticket.ExportMatrix:       "katilim-matrisi",
	ticket.ExportEntries:      "alana-giris",
	ticket.ExportCertificates: "sertifikalar",
}

// NeedsStandings reports whether the report counts attendance by the
// certificate rule (certificate.Service.Standings).
func NeedsStandings(report string) bool {
	return report == ticket.ExportAttendees || report == ticket.ExportMatrix || report == ticket.ExportCertificates
}

// Build lays out the report d asks for. standings is the Event's
// attendance by its certificate rule, read when NeedsStandings.
func Build(d *ticket.ExportData, standings *certificate.Standings) (Table, error) {
	b := newBuilder(d, standings)
	switch d.Request.Report {
	case ticket.ExportAttendees:
		return b.attendees(), nil
	case ticket.ExportSession:
		return b.session(), nil
	case ticket.ExportMatrix:
		return b.matrix(), nil
	case ticket.ExportEntries:
		return b.entries(), nil
	case ticket.ExportCertificates:
		return b.certificates(), nil
	}
	return Table{}, fmt.Errorf("attendeeexport: unknown report %q", d.Request.Report)
}

type builder struct {
	d         *ticket.ExportData
	standings *certificate.Standings
	// program is the live (not cancelled) Sessions in the program's order.
	program  []event.Session
	byID     map[uuid.UUID]event.Session
	dayOf    map[uuid.UUID]event.Day
	entered  map[uuid.UUID][]ticket.DoorEntry
	collator *collate.Collator
}

func newBuilder(d *ticket.ExportData, standings *certificate.Standings) *builder {
	b := &builder{
		d: d, standings: standings, byID: map[uuid.UUID]event.Session{}, dayOf: map[uuid.UUID]event.Day{},
		entered: map[uuid.UUID][]ticket.DoorEntry{}, collator: collate.New(language.Turkish, collate.IgnoreCase),
	}
	dayIndex := map[uuid.UUID]int{}
	for i, day := range d.Days {
		dayIndex[day.ID] = i
		b.dayOf[day.ID] = day
	}
	for _, sess := range d.Sessions {
		b.byID[sess.ID] = sess
		if !sess.Cancelled {
			b.program = append(b.program, sess)
		}
	}
	slices.SortStableFunc(b.program, func(a, c event.Session) int {
		if x, y := dayIndex[a.EventDayID], dayIndex[c.EventDayID]; x != y {
			return x - y
		}
		if a.OrderIndex != c.OrderIndex {
			return a.OrderIndex - c.OrderIndex
		}
		switch {
		case a.StartTime == nil || c.StartTime == nil:
			return 0
		default:
			return a.StartTime.Compare(*c.StartTime)
		}
	})
	for _, e := range d.Entries {
		b.entered[e.TicketID] = append(b.entered[e.TicketID], e)
	}
	for id := range b.entered {
		slices.SortFunc(b.entered[id], func(a, c ticket.DoorEntry) int { return a.CreatedAt.Compare(c.CreatedAt) })
	}
	return b
}

// person columns open every report: who, and whether a member.
var personColumns = []Column{
	{Header: "Ad", Width: 16}, {Header: "Soyad", Width: 16}, {Header: "E-posta", Width: 30}, {Header: "Üye/Misafir", Width: 12},
}

func (b *builder) person(t ticket.Ticket) []any {
	p := b.d.People[t.ID]
	return []any{p.FirstName, p.LastName, p.Email, typeLabel(t)}
}

// contactColumns follow the person in the reports about the people
// themselves (Katılımcılar, Oturum katılımı, Alana giriş): a guest's phone
// (a text cell, never a number or a formula), and the university,
// department and grade a guest gave or a member's account holds.
var contactColumns = []Column{
	{Header: "Telefon", Width: 16}, {Header: "Üniversite", Width: 24}, {Header: "Bölüm", Width: 24}, {Header: "Sınıf", Width: 12},
}

func (b *builder) contact(t ticket.Ticket) []any {
	p := b.d.People[t.ID]
	return []any{p.Phone, p.University, p.Department, gradeLabels[p.Grade]}
}

// gradeLabels are the grade keys as the panel's Kapıda kayıt shows them.
var gradeLabels = map[string]string{
	"prep": "Hazırlık", "1": "1", "2": "2", "3": "3", "4": "4",
	"masters": "Yüksek lisans", "phd": "Doktora", "graduate": "Mezun", "other": "Diğer",
}

// withContact is the person's cells and their contact.
func (b *builder) withContact(t ticket.Ticket) []any {
	return append(b.person(t), b.contact(t)...)
}

// personAndContact are the person's columns and their contact's.
func personAndContact() []Column {
	return append(slices.Clone(personColumns), contactColumns...)
}

func typeLabel(t ticket.Ticket) string {
	if t.TicketType == ticket.Registered {
		return "Üye"
	}
	return "Misafir"
}

func statusLabel(t ticket.Ticket) string {
	switch t.StatusOf() {
	case ticket.StatusWaitlisted:
		return "Bekleme listesi"
	case ticket.StatusCancelled:
		return "İptal"
	default:
		return "Aktif"
	}
}

// sourceLabel is where the registration came from, as core knows it: an
// accepted Forms answer, the door (a Walk-in, which entered with it), or
// directly (the person's own application, the panel's Katılımcı ekle or an
// import).
func (b *builder) sourceLabel(t ticket.Ticket) string {
	if t.FormsStatus != "" {
		return "Form"
	}
	for _, e := range b.entered[t.ID] {
		if e.Source == ticket.EntryWalkIn {
			return "Kapıda kayıt"
		}
	}
	return "Doğrudan"
}

var certificateLabels = map[string]string{
	certificate.StatusIssued:   "Verildi",
	certificate.StatusQueued:   "Kuyrukta",
	certificate.StatusFailed:   "Hata",
	certificate.StatusRevoked:  "İptal edildi",
	certificate.StatusEligible: "Uygun, verilmedi",
	certificate.StatusBelow:    "Eşik altı",
}

func yesNo(v bool) string {
	if v {
		return "Evet"
	}
	return "Hayır"
}

// ruled reports whether the Event has a certificate rule to count by.
func (b *builder) ruled() bool {
	return b.standings != nil && b.standings.Rule != event.RuleNone && b.standings.Rule != ""
}

// attended is how many live Sessions t checked in on, counted or not.
func (b *builder) attended(t ticket.Ticket) int {
	seen := map[uuid.UUID]bool{}
	for _, ci := range t.CheckIns {
		if sess, ok := b.byID[ci.SessionID]; ok && !sess.Cancelled {
			seen[ci.SessionID] = true
		}
	}
	return len(seen)
}

func (b *builder) standing(t ticket.Ticket) (certificate.Standing, bool) {
	if b.standings == nil {
		return certificate.Standing{}, false
	}
	s, ok := b.standings.Tickets[t.ID]
	return s, ok
}

// countColumns and countCells are the certificate rule's counts, and the
// certificate's status for a reader of certificates.
func (b *builder) countColumns() []Column {
	cols := []Column{
		{Header: "Sayılan oturumlardan katıldığı", Kind: Number, Width: 14},
		{Header: "Sayılan oturum sayısı", Kind: Number, Width: 12},
		{Header: "Sertifika eşiğini geçiyor", Width: 14},
	}
	if b.d.Certificates {
		cols = append(cols, Column{Header: "Sertifika durumu", Width: 16})
	}
	return cols
}

func (b *builder) countCells(t ticket.Ticket) []any {
	s, ok := b.standing(t)
	counted := 0
	if b.standings != nil {
		counted = b.standings.CountedTotal
	}
	cells := []any{nil, counted, ""}
	if ok {
		cells[0] = s.Attended
		if b.ruled() {
			cells[2] = yesNo(s.Eligible)
		} else {
			cells[2] = "Kural yok"
		}
	} else {
		// Not active: neither counted nor eligible.
		cells[1] = nil
	}
	if b.d.Certificates {
		label := ""
		if ok {
			label = certificateLabels[s.Status]
		}
		cells = append(cells, label)
	}
	return cells
}

// ordered are the report's Tickets: the panel's order for a selection,
// else by name (Turkish collation), or as listed (by registration) when
// byRegistration.
func (b *builder) ordered(keep func(ticket.Ticket) bool, byRegistration bool) []ticket.Ticket {
	out := make([]ticket.Ticket, 0, len(b.d.Tickets))
	for _, t := range b.d.Tickets {
		if keep == nil || keep(t) {
			out = append(out, t)
		}
	}
	if b.d.Request.Selected {
		place := make(map[uuid.UUID]int, len(b.d.Request.TicketIDs))
		for i, id := range b.d.Request.TicketIDs {
			place[id] = i
		}
		slices.SortStableFunc(out, func(a, c ticket.Ticket) int { return place[a.ID] - place[c.ID] })
		return out
	}
	if byRegistration {
		slices.SortStableFunc(out, func(a, c ticket.Ticket) int { return a.CreatedAt.Compare(c.CreatedAt) })
		return out
	}
	slices.SortStableFunc(out, func(a, c ticket.Ticket) int {
		pa, pc := b.d.People[a.ID], b.d.People[c.ID]
		if n := b.collator.CompareString(pa.FirstName+" "+pa.LastName, pc.FirstName+" "+pc.LastName); n != 0 {
			return n
		}
		return a.CreatedAt.Compare(c.CreatedAt)
	})
	return out
}

func active(t ticket.Ticket) bool { return t.Active() }

// attendees is the ticket list: each Ticket, its status and registration,
// its Venue entries and its attendance.
func (b *builder) attendees() Table {
	cols := append(personAndContact(),
		Column{Header: "Bilet kodu", Width: 12},
		Column{Header: "Bilet durumu", Width: 14},
		Column{Header: "Kayıt kaynağı", Width: 12},
		Column{Header: "Kayıt tarihi", Kind: DateTime, Width: 17},
		Column{Header: "Alana giriş (gün)", Kind: Number, Width: 10},
		Column{Header: "Alana giriş zamanları", Width: 30},
		Column{Header: "Katıldığı oturum sayısı", Kind: Number, Width: 12},
	)
	cols = append(cols, b.countColumns()...)
	rows := make([][]any, 0, len(b.d.Tickets))
	for _, t := range b.ordered(nil, true) {
		entered := b.entered[t.ID]
		times := make([]string, len(entered))
		for i, e := range entered {
			times[i] = FormatTime(e.CreatedAt)
		}
		created := t.CreatedAt
		row := append(b.withContact(t), t.Code, statusLabel(t), b.sourceLabel(t), &created, len(entered), strings.Join(times, ", "), b.attended(t))
		rows = append(rows, append(row, b.countCells(t)...))
	}
	return Table{Sheet: reportNames[ticket.ExportAttendees], Columns: cols, Rows: rows}
}

// sessionLabel names a Session: its day (name or date), start and title.
func (b *builder) sessionLabel(s event.Session) string {
	parts := []string{}
	day := b.dayOf[s.EventDayID]
	switch {
	case strings.TrimSpace(day.Name) != "":
		parts = append(parts, strings.TrimSpace(day.Name))
	case day.StartDate != nil:
		parts = append(parts, day.StartDate.In(istanbul).Format("02.01"))
	}
	if s.StartTime != nil {
		parts = append(parts, s.StartTime.In(istanbul).Format("15:04"))
	}
	parts = append(parts, strings.TrimSpace(s.Title))
	return strings.Join(parts, " ")
}

// session is one Session's attendance: who checked in, and when.
func (b *builder) session() Table {
	id := b.d.Request.SessionID
	sess := b.byID[id]
	cols := append(personAndContact(),
		Column{Header: "Bilet durumu", Width: 14},
		Column{Header: "Katılım zamanı", Kind: DateTime, Width: 17},
		Column{Header: "Oturum", Width: 36},
	)
	label := b.sessionLabel(sess)
	when := map[uuid.UUID]time.Time{}
	keep := func(t ticket.Ticket) bool {
		for _, ci := range t.CheckIns {
			if ci.SessionID == id {
				when[t.ID] = ci.CreatedAt
				return true
			}
		}
		return false
	}
	rows := [][]any{}
	for _, t := range b.ordered(keep, false) {
		at := when[t.ID]
		rows = append(rows, append(b.withContact(t), statusLabel(t), &at, label))
	}
	return Table{Sheet: sheetName(reportNames[ticket.ExportSession]), Columns: cols, Rows: rows}
}

// Check is a matrix cell of a Session attended.
const Check = "✓"

// matrix is person × Session: a mark where they checked in, their totals
// and whether they pass the certificate threshold.
func (b *builder) matrix() Table {
	cols := slices.Clone(personColumns)
	for _, s := range b.program {
		header := b.sessionLabel(s)
		if s.CertificateCounted {
			header += " (sayılır)"
		}
		cols = append(cols, Column{Header: header, Width: 16})
	}
	cols = append(cols, Column{Header: "Toplam katıldığı oturum", Kind: Number, Width: 12})
	cols = append(cols, b.countColumns()...)
	rows := [][]any{}
	for _, t := range b.ordered(active, false) {
		attended := map[uuid.UUID]bool{}
		for _, ci := range t.CheckIns {
			attended[ci.SessionID] = true
		}
		row := b.person(t)
		for _, s := range b.program {
			if attended[s.ID] {
				row = append(row, Check)
			} else {
				row = append(row, "")
			}
		}
		row = append(row, b.attended(t))
		rows = append(rows, append(row, b.countCells(t)...))
	}
	return Table{Sheet: reportNames[ticket.ExportMatrix], Columns: cols, Rows: rows}
}

var entrySourceLabels = map[ticket.EntrySource]string{
	ticket.EntryScan:       "QR okutma",
	ticket.EntryDesk:       "Kapı listesi (elle)",
	ticket.EntryAttendance: "Oturum katılımıyla",
	ticket.EntryWalkIn:     "Kapıda kayıt",
}

// entries is the Venue entries, day by day.
func (b *builder) entries() Table {
	cols := []Column{{Header: "Gün", Width: 11}}
	cols = append(cols, personAndContact()...)
	cols = append(cols,
		Column{Header: "Giriş zamanı", Kind: DateTime, Width: 17},
		Column{Header: "Cihazda okutma zamanı", Kind: DateTime, Width: 17},
		Column{Header: "Yol", Width: 18},
	)
	tickets := make(map[uuid.UUID]ticket.Ticket, len(b.d.Tickets))
	for _, t := range b.d.Tickets {
		tickets[t.ID] = t
	}
	list := slices.Clone(b.d.Entries)
	slices.SortStableFunc(list, func(a, c ticket.DoorEntry) int {
		if a.EntryDate != c.EntryDate {
			return strings.Compare(a.EntryDate, c.EntryDate)
		}
		return a.CreatedAt.Compare(c.CreatedAt)
	})
	rows := [][]any{}
	for _, e := range list {
		t, ok := tickets[e.TicketID]
		if !ok {
			// A selection leaves the others' entries out.
			continue
		}
		day := e.EntryDate
		if d, err := time.Parse("2006-01-02", e.EntryDate); err == nil {
			day = d.Format("02.01.2006")
		}
		created := e.CreatedAt
		row := []any{day}
		row = append(row, b.withContact(t)...)
		row = append(row, &created, e.ScannedAt, entrySourceLabels[e.Source])
		rows = append(rows, row)
	}
	return Table{Sheet: reportNames[ticket.ExportEntries], Columns: cols, Rows: rows}
}

// certificates is the certificate list: every active Ticket's attendance,
// status and certificate.
func (b *builder) certificates() Table {
	cols := append(slices.Clone(personColumns),
		Column{Header: "Sayılan oturumlardan katıldığı", Kind: Number, Width: 14},
		Column{Header: "Sayılan oturum sayısı", Kind: Number, Width: 12},
		Column{Header: "Sertifika durumu", Width: 16},
		Column{Header: "Seri no", Width: 18},
		Column{Header: "Veriliş", Kind: DateTime, Width: 17},
		Column{Header: "İptal", Kind: DateTime, Width: 17},
	)
	rows := [][]any{}
	for _, t := range b.ordered(active, false) {
		s, _ := b.standing(t)
		counted := 0
		if b.standings != nil {
			counted = b.standings.CountedTotal
		}
		row := append(b.person(t), s.Attended, counted, certificateLabels[s.Status])
		if c := s.Certificate; c != nil {
			issued := c.IssuedAt
			row = append(row, c.Serial, &issued, c.RevokedAt)
		} else {
			row = append(row, "", nil, nil)
		}
		rows = append(rows, row)
	}
	return Table{Sheet: reportNames[ticket.ExportCertificates], Columns: cols, Rows: rows}
}
