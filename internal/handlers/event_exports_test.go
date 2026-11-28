package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/attendeeexport"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
	"github.com/xuri/excelize/v2"
)

type exportFixture struct {
	ev       event.Event
	sessions []event.Session
	tickets  map[string]ticket.Ticket
	audit    *teamaudit.Memory
	app      func(authn.Identity) *fiber.App
	logs     *bytes.Buffer
	lead     uuid.UUID
	staff    uuid.UUID
	store    *ticket.MemoryStore
}

// newExportFixture is an Event of WEBLAB with two counted Sessions and
// one not counted, a member and three guests (one cancelled), check-ins
// and a Venue entry, read by the real ticket and certificate services.
func newExportFixture(t *testing.T) exportFixture {
	t.Helper()
	ctx := t.Context()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	audit := teamaudit.NewMemory()
	events := event.NewMemoryStore()
	store := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	staff := uuid.New()
	ev, err := events.Create(ctx, event.Event{Name: "ARTLAB Günü 2026", Location: "YTÜ", OwnerTeam: "WEBLAB",
		AttendanceRule: event.RuleCount, DoorStaffIDs: []uuid.UUID{staff}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "1. gün", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	var sessions []event.Session
	for i, title := range []string{"Açılış", "Atölye", "Kahve arası"} {
		at := start.Add(time.Duration(i) * time.Hour)
		sess, err := events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: title, StartTime: &at, OrderIndex: i,
			CertificateCounted: i < 2})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, sess)
	}
	memberID := uuid.New()
	if _, _, err := users.Upsert(ctx, user.User{ID: memberID, Email: "ilke@example.com", FirstName: "İlke", LastName: "Şahin"}); err != nil {
		t.Fatal(err)
	}
	tickets := map[string]ticket.Ticket{}
	add := func(key string, tk ticket.Ticket) {
		tk.EventID = ev.ID
		made, err := store.Create(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		tickets[key] = made
	}
	add("member", ticket.Ticket{TicketType: ticket.Registered, OwnerID: &memberID})
	add("ada", ticket.Ticket{TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com", GuestPhoneNumber: "+905551112233",
		GuestUniversity: "Yıldız Teknik Üniversitesi", GuestDepartment: "Matematik", GuestGrade: "masters"})
	add("formula", ticket.Ticket{TicketType: ticket.Guest, GuestFirstName: "=HYPERLINK(\"x\")", GuestLastName: "Çelik", GuestEmail: "c@example.com"})
	add("gone", ticket.Ticket{TicketType: ticket.Guest, GuestFirstName: "Can", GuestLastName: "İptal", GuestEmail: "can@example.com"})
	if _, _, err := store.Cancel(ctx, tickets["gone"].ID, ticket.CancelledByOperator, ""); err != nil {
		t.Fatal(err)
	}
	for _, ci := range []struct {
		key string
		s   int
	}{{"member", 0}, {"member", 1}, {"ada", 0}, {"ada", 2}} {
		if _, err := store.AddCheckIn(ctx, ticket.CheckIn{TicketID: tickets[ci.key].ID, SessionID: sessions[ci.s].ID, EventDayID: day.ID}); err != nil {
			t.Fatal(err)
		}
	}
	scannedAt := time.Date(2026, 11, 27, 6, 58, 0, 0, time.UTC)
	if _, err := store.AddVenueEntry(ctx, ticket.VenueEntry{TicketID: tickets["ada"].ID, EntryDate: "2026-11-27", Source: ticket.EntryScan, ScannedAt: &scannedAt}); err != nil {
		t.Fatal(err)
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	svc := ticket.NewService(store, events, az, users, dir, audit)
	certs := certificate.NewService(certificate.NewMemoryStore(), store, events, users, az, nil, nil, "https://api.example.test")
	logs := &bytes.Buffer{}
	f := exportFixture{ev: ev, sessions: sessions, tickets: tickets, audit: audit, logs: logs, lead: uuid.New(), staff: staff, store: store}
	f.app = func(ident authn.Identity) *fiber.App {
		h := NewExportHandler(svc, certs)
		h.logger = log.New(logs, "", 0)
		h.now = func() time.Time { return time.Date(2026, 11, 26, 21, 5, 0, 0, time.UTC) }
		a := fiber.New()
		a.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, ident)
			return c.Next()
		})
		a.Post("/v1/events/:eventId/exports", h.Export)
		return a
	}
	return f
}

type exportAnswer struct {
	status int
	header map[string]string
	body   []byte
}

func (f exportFixture) call(t *testing.T, a *fiber.App, body any) exportAnswer {
	t.Helper()
	raw, ok := body.(string)
	if !ok {
		b, _ := json.Marshal(body)
		raw = string(b)
	}
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+f.ev.ID.String()+"/exports", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	h := map[string]string{}
	for _, k := range []string{"Content-Type", "Content-Disposition", "Cache-Control", "X-Content-Type-Options"} {
		h[k] = resp.Header.Get(k)
	}
	return exportAnswer{status: resp.StatusCode, header: h, body: got}
}

func (f exportFixture) leader() authn.Identity {
	return authn.Identity{ID: f.lead, Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
}

func sheetRows(t *testing.T, body []byte) [][]string {
	t.Helper()
	wb, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Close()
	rows, err := wb.GetRows(wb.GetSheetName(0))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Who may export: a reader of the Event's Tickets (its team's Leader);
// never its door staff, another team's member, or nobody.
func TestAttendeeExportWho(t *testing.T) {
	t.Parallel()
	f := newExportFixture(t)
	body := map[string]any{"report": "attendees", "format": "csv"}
	if got := f.call(t, f.app(f.leader()), body); got.status != fiber.StatusOK {
		t.Fatalf("leader %d %s", got.status, got.body)
	}
	for name, ident := range map[string]authn.Identity{
		"door staff":          {ID: f.staff, Groups: []string{"/UYELER/ARGE/GAMELAB"}},
		"another team":        {ID: uuid.New(), Groups: []string{"/UYELER/ARGE/GAMELAB"}},
		"another team's lead": {ID: uuid.New(), Groups: []string{"/UYELER/ARGE/GAMELAB/LIDERLER"}},
	} {
		if got := f.call(t, f.app(ident), body); got.status != fiber.StatusForbidden {
			t.Errorf("%s: %d %s", name, got.status, got.body)
		}
	}
	if got := f.call(t, f.app(authn.Identity{}), body); got.status != fiber.StatusUnauthorized && got.status != fiber.StatusForbidden {
		t.Errorf("nobody: %d", got.status)
	}
	// Only the leader's export was recorded.
	if rows := f.audit.Rows(); len(rows) != 1 {
		t.Fatalf("%d audit rows", len(rows))
	}
}

// Each export is a ticket.export row under the caller, closed with what
// was downloaded and never a row's content; the log line names nobody.
func TestAttendeeExportAudit(t *testing.T) {
	t.Parallel()
	f := newExportFixture(t)
	a := f.app(f.leader())
	got := f.call(t, a, map[string]any{"report": "session", "format": "xlsx", "sessionId": f.sessions[0].ID})
	if got.status != fiber.StatusOK {
		t.Fatalf("%d %s", got.status, got.body)
	}
	rows := f.audit.Rows()
	if len(rows) != 1 {
		t.Fatalf("%d audit rows", len(rows))
	}
	r := rows[0]
	if r.Action != teamaudit.ActionTicketExport || r.Actor.ID != f.lead || r.Outcome != teamaudit.OutcomeOK ||
		r.Actor.Basis != teamaudit.BasisLeader || r.After == nil || r.After.EventID != f.ev.ID.String() {
		t.Fatalf("row %+v", r)
	}
	if x := r.After.Export; x == nil || x.Report != "session" || x.Format != "xlsx" || x.Rows != 2 || x.SessionID != f.sessions[0].ID.String() || x.Selected {
		t.Fatalf("export %+v", x)
	}
	raw, _ := json.Marshal(r)
	if s := string(raw); strings.Contains(s, "@") || strings.Contains(s, "Ada") {
		t.Fatalf("the row names someone: %s", s)
	}
	if line := f.logs.String(); !strings.Contains(line, `"attendees_exported"`) || strings.Contains(line, "@") || strings.Contains(line, "Ada") ||
		strings.Contains(line, f.lead.String()) {
		t.Fatalf("log %q", line)
	}
}

// The CSV: BOM, semicolons, Turkish headers, Istanbul's clock, the
// formula-looking name and the phone as text, the grade in Turkish; the
// answer is an attachment kept by no cache.
func TestAttendeeExportCSV(t *testing.T) {
	t.Parallel()
	f := newExportFixture(t)
	got := f.call(t, f.app(f.leader()), map[string]any{"report": "attendees", "format": "csv"})
	if got.status != fiber.StatusOK {
		t.Fatalf("%d %s", got.status, got.body)
	}
	if got.header["Cache-Control"] != "no-store" || got.header["X-Content-Type-Options"] != "nosniff" ||
		!strings.HasPrefix(got.header["Content-Type"], "text/csv") ||
		got.header["Content-Disposition"] != `attachment; filename=artlab-gunu-2026-katilimcilar-20261127-0005.csv` {
		t.Fatalf("headers %v", got.header)
	}
	text := string(got.body)
	if !strings.HasPrefix(text, attendeeexport.BOM+"Ad;Soyad;E-posta;Üye/Misafir;Telefon;Üniversite;Bölüm;Sınıf;Bilet kodu;Bilet durumu;Kayıt kaynağı;Kayıt tarihi;") {
		t.Fatalf("head %.200q", text)
	}
	for _, want := range []string{"İlke;Şahin;ilke@example.com;Üye", "Ada;Lovelace;ada@example.com;Misafir;'+905551112233;Yıldız Teknik Üniversitesi;Matematik;Yüksek lisans;",
		`'=HYPERLINK`, "Can;İptal;can@example.com;Misafir;", ";İptal;Doğrudan;",
		"Sertifika eşiğini geçiyor", "Sertifika durumu"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	lines := strings.Split(strings.TrimSpace(text), "\r\n")
	if len(lines) != 5 {
		t.Fatalf("%d lines", len(lines))
	}
	// Ada: entered once, attended the two live Sessions she checked in
	// on, one of them counted (of 2): below the count rule's threshold.
	for _, line := range lines {
		if strings.HasPrefix(line, "Ada;") {
			cells := strings.Split(line, ";")
			if cells[12] != "1" || cells[14] != "2" || cells[15] != "1" || cells[16] != "2" || cells[17] != "Hayır" || cells[18] != "Eşik altı" {
				t.Errorf("Ada %q", cells)
			}
		}
		if strings.HasPrefix(line, "İlke;") {
			cells := strings.Split(line, ";")
			if cells[15] != "2" || cells[17] != "Evet" || cells[18] != "Uygun, verilmedi" {
				t.Errorf("İlke %q", cells)
			}
		}
	}
}

// The workbook: one sheet, header row frozen with an auto filter, the
// matrix's ✓ per Session in the program's order, and only the rows the
// panel showed, in its order, when it names them.
func TestAttendeeExportXLSX(t *testing.T) {
	t.Parallel()
	f := newExportFixture(t)
	a := f.app(f.leader())
	got := f.call(t, a, map[string]any{"report": "matrix", "format": "xlsx"})
	if got.status != fiber.StatusOK || got.header["Content-Type"] != attendeeexport.XLSXType {
		t.Fatalf("%d %v %.200s", got.status, got.header, got.body)
	}
	wb, err := excelize.OpenReader(bytes.NewReader(got.body))
	if err != nil {
		t.Fatal(err)
	}
	sheet := wb.GetSheetName(0)
	if sheet != "Katılım matrisi" {
		t.Fatalf("sheet %q", sheet)
	}
	panes, err := wb.GetPanes(sheet)
	if err != nil || !panes.Freeze || panes.YSplit != 1 {
		t.Fatalf("panes %+v %v", panes, err)
	}
	filtered := false
	for _, name := range wb.GetDefinedName() {
		filtered = filtered || (name.Name == "_xlnm._FilterDatabase" && strings.Contains(name.RefersTo, "$A$1:"))
	}
	if !filtered {
		t.Fatalf("no auto filter: %+v", wb.GetDefinedName())
	}
	wb.Close()
	rows := sheetRows(t, got.body)
	head := strings.Join(rows[0], "|")
	if !strings.Contains(head, "1. gün 10:00 Açılış (sayılır)|1. gün 11:00 Atölye (sayılır)|1. gün 12:00 Kahve arası|Toplam katıldığı oturum") {
		t.Fatalf("head %s", head)
	}
	// Active Tickets only (the cancelled one left out), by name.
	if len(rows) != 4 {
		t.Fatalf("%d rows", len(rows))
	}
	byName := map[string][]string{}
	for _, r := range rows[1:] {
		byName[r[0]] = r
	}
	if r := byName["Ada"]; len(r) < 7 || r[4] != attendeeexport.Check || r[5] != "" || r[6] != attendeeexport.Check || r[7] != "2" {
		t.Fatalf("Ada %q", r)
	}
	if r := byName["İlke"]; len(r) < 7 || r[4] != attendeeexport.Check || r[5] != attendeeexport.Check || r[9] != "2" || r[10] != "Evet" {
		t.Fatalf("İlke %q", r)
	}
	if _, ok := byName[`=HYPERLINK("x")`]; !ok || byName["Can"] != nil {
		t.Fatalf("rows %q", rows)
	}

	// The panel's rows: İlke then Ada; the cancelled one it does not name
	// stays out, and an id of no Ticket of the Event is ignored.
	got = f.call(t, a, map[string]any{"report": "attendees", "format": "xlsx",
		"ticketIds": []uuid.UUID{f.tickets["member"].ID, uuid.New(), f.tickets["ada"].ID}})
	if got.status != fiber.StatusOK {
		t.Fatalf("%d %s", got.status, got.body)
	}
	rows = sheetRows(t, got.body)
	if len(rows) != 3 || rows[1][0] != "İlke" || rows[2][0] != "Ada" {
		t.Fatalf("selected %q", rows)
	}
	// The phone is a text cell, as given; the registration a date cell.
	if rows[2][4] != "+905551112233" || rows[2][7] != "Yüksek lisans" || !strings.Contains(rows[2][11], "2026") {
		t.Fatalf("Ada %q", rows[2])
	}
	last := f.audit.Rows()
	if x := last[len(last)-1].After.Export; !x.Selected || x.Rows != 2 {
		t.Fatalf("selected row %+v", x)
	}
}

// The Venue entries and certificates reports, and requests no report
// answers.
func TestAttendeeExportReportsAndRefusals(t *testing.T) {
	t.Parallel()
	f := newExportFixture(t)
	a := f.app(f.leader())
	got := f.call(t, a, map[string]any{"report": "entries", "format": "csv"})
	if got.status != fiber.StatusOK || !strings.Contains(string(got.body), "27.11.2026;Ada;Lovelace;ada@example.com;Misafir;'+905551112233;Yıldız Teknik Üniversitesi;Matematik;Yüksek lisans;") ||
		!strings.Contains(string(got.body), ";27.11.2026 09:58;QR okutma") {
		t.Fatalf("entries %d %s", got.status, got.body)
	}
	got = f.call(t, a, map[string]any{"report": "certificates", "format": "csv"})
	if got.status != fiber.StatusOK || !strings.Contains(string(got.body), "Seri no") || !strings.Contains(string(got.body), "İlke;Şahin;ilke@example.com;Üye;2;2;Uygun, verilmedi;") {
		t.Fatalf("certificates %d %s", got.status, got.body)
	}
	before := len(f.audit.Rows())
	for name, body := range map[string]any{
		"no report":           map[string]any{"format": "csv"},
		"unknown format":      map[string]any{"report": "attendees", "format": "pdf"},
		"session without one": map[string]any{"report": "session"},
		"session on another":  map[string]any{"report": "attendees", "sessionId": f.sessions[0].ID},
		"bad body":            `{"report":`,
	} {
		if got := f.call(t, a, body); got.status != fiber.StatusBadRequest {
			t.Errorf("%s: %d %s", name, got.status, got.body)
		}
	}
	if got := f.call(t, a, map[string]any{"report": "session", "sessionId": uuid.New()}); got.status != fiber.StatusNotFound {
		t.Errorf("another Event's Session: %d", got.status)
	}
	if after := len(f.audit.Rows()); after != before {
		t.Errorf("a refused export wrote %d rows", after-before)
	}
}

// Without the audit actor (a route that passed c.Context()) the export
// would answer 503 and download nothing; the route passes auditContext.
func TestAttendeeExportNeedsTheAuditRow(t *testing.T) {
	t.Parallel()
	f := newExportFixture(t)
	f.audit.SetUnavailable(true)
	got := f.call(t, f.app(f.leader()), map[string]any{"report": "attendees"})
	if got.status != fiber.StatusServiceUnavailable || !strings.Contains(string(got.body), "audit_unavailable") || bytes.Contains(got.body, []byte("Ada")) {
		t.Fatalf("%d %s", got.status, got.body)
	}
}

// A 2 000-Ticket Event exports its matrix in one request, quickly.
func TestAttendeeExportLargeEvent(t *testing.T) {
	t.Parallel()
	f := newExportFixture(t)
	ctx := t.Context()
	// The fixture's stores are inside its services; reach the ticket store
	// through a second fixture-like setup.
	events := event.NewMemoryStore()
	store := ticket.NewMemoryStore()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	ev, err := events.Create(ctx, event.Event{Name: "Büyük", Location: "YTÜ", OwnerTeam: "WEBLAB", AttendanceRule: event.RuleOnce})
	if err != nil {
		t.Fatal(err)
	}
	day, _ := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	var sessions []event.Session
	for i := range 5 {
		sess, _ := events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: fmt.Sprintf("Oturum %d", i), OrderIndex: i, CertificateCounted: true})
		sessions = append(sessions, sess)
	}
	for i := range 2000 {
		tk, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: fmt.Sprintf("Kişi%04d", i), GuestEmail: fmt.Sprintf("k%d@example.com", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AddCheckIn(ctx, ticket.CheckIn{TicketID: tk.ID, SessionID: sessions[i%5].ID, EventDayID: day.ID}); err != nil {
			t.Fatal(err)
		}
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	users := user.NewMemoryStore()
	h := NewExportHandler(ticket.NewService(store, events, az, users, dir, teamaudit.NewMemory()),
		certificate.NewService(certificate.NewMemoryStore(), store, events, users, az, nil, nil, "https://api.example.test"))
	h.logger = nil
	a := fiber.New()
	a.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, f.leader())
		return c.Next()
	})
	a.Post("/v1/events/:eventId/exports", h.Export)
	f.ev = ev
	start := time.Now()
	got := f.call(t, a, map[string]any{"report": "matrix", "format": "xlsx"})
	if got.status != fiber.StatusOK {
		t.Fatalf("%d %.300s", got.status, got.body)
	}
	// A loose bound: CI runs the tests with -race on a shared machine,
	// where 3 000 rows take seconds; it only catches a quadratic slip.
	if took := time.Since(start); took > time.Minute {
		t.Fatalf("took %s", took)
	}
	if rows := sheetRows(t, got.body); len(rows) != 2001 || rows[1][len(rows[1])-2] != "Evet" {
		t.Fatalf("%d rows; %q", len(rows), rows[1])
	}
	t.Logf("2000 tickets: %d bytes in %s", len(got.body), time.Since(start))
}

// A Walk-in's Ticket says so: its registration came from the door, and its
// Venue entry's way is Kapıda kayıt.
func TestAttendeeExportWalkIn(t *testing.T) {
	t.Parallel()
	f := newExportFixture(t)
	ctx := t.Context()
	walked, err := f.store.Create(ctx, ticket.Ticket{EventID: f.ev.ID, TicketType: ticket.Guest, GuestFirstName: "Deniz", GuestLastName: "Kapı",
		GuestEmail: "deniz@example.com", GuestGrade: "prep"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AddVenueEntry(ctx, ticket.VenueEntry{TicketID: walked.ID, EntryDate: "2026-11-27", Source: ticket.EntryWalkIn}); err != nil {
		t.Fatal(err)
	}
	a := f.app(f.leader())
	got := f.call(t, a, map[string]any{"report": "attendees", "format": "csv"})
	if got.status != fiber.StatusOK || !strings.Contains(string(got.body), "Deniz;Kapı;deniz@example.com;Misafir;;;;Hazırlık;") ||
		!strings.Contains(string(got.body), ";Aktif;Kapıda kayıt;") {
		t.Fatalf("attendees %d %s", got.status, got.body)
	}
	got = f.call(t, a, map[string]any{"report": "entries", "format": "csv"})
	if got.status != fiber.StatusOK || !strings.Contains(string(got.body), ";Kapıda kayıt\r\n") {
		t.Fatalf("entries %d %s", got.status, got.body)
	}
}

// The door staff report (docs/door-staff-stats.md): a row per person who
// wrote at the door, by name and never by e-mail, then the people's own;
// the door staff themselves cannot download it, and it takes no Ticket
// selection.
func TestAttendeeExportDoorStaff(t *testing.T) {
	t.Parallel()
	f := newExportFixture(t)
	ctx := t.Context()
	staff := f.staff
	if _, err := f.store.AddVenueEntry(ctx, ticket.VenueEntry{TicketID: f.tickets["member"].ID, EntryDate: "2026-11-27", Source: ticket.EntryDesk, ByUserID: &staff}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AddVenueEntry(ctx, ticket.VenueEntry{TicketID: f.tickets["formula"].ID, EntryDate: "2026-11-27", Source: ticket.EntryAttendance}); err != nil {
		t.Fatal(err)
	}
	a := f.app(f.leader())
	got := f.call(t, a, map[string]any{"report": "door_staff", "format": "csv"})
	body := string(got.body)
	if got.status != fiber.StatusOK || !strings.Contains(got.header["Content-Disposition"], "kapi-gorevlileri") ||
		!strings.Contains(body, "Görevli;Alana giriş;Giriş: tarama;Giriş: masa") ||
		!strings.Contains(body, "Silinmiş kullanıcı;1;0;1;0;0;0;0;0;0;0;") ||
		!strings.Contains(body, "Yazanı bilinmeyen;1;0;0;0;4;") ||
		!strings.Contains(body, "Katılımcının kendisi (oturum QR);1;") || strings.Contains(body, "@") {
		t.Fatalf("door staff %d %s", got.status, body)
	}
	rows := f.audit.Rows()
	if last := rows[len(rows)-1]; last.Action != teamaudit.ActionTicketExport || last.After == nil || last.After.Export == nil || last.After.Export.Report != "door_staff" {
		t.Fatalf("audit %+v", last)
	}
	if got := f.call(t, a, map[string]any{"report": "door_staff", "ticketIds": []uuid.UUID{f.tickets["ada"].ID}}); got.status != fiber.StatusBadRequest {
		t.Fatalf("selected: %d", got.status)
	}
	if got := f.call(t, f.app(authn.Identity{ID: f.staff}), map[string]any{"report": "door_staff"}); got.status != fiber.StatusForbidden {
		t.Fatalf("door staff: %d %s", got.status, got.body)
	}
}
