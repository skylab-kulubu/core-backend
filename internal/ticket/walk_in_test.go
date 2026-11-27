package ticket_test

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// mailingStore is the memory store with the new Ticket's mail on, as
// PostgresStore while TICKET_CREATED_NOTIFICATION is on.
type mailingStore struct{ *ticket.MemoryStore }

func (mailingStore) NotifiesCreated() bool { return true }

// walkInDoor is a WEBLAB Event (venue mode unless said) with its door staff,
// a guest Ticket and a member's Ticket, the team directory and the audit.
type walkInDoor struct {
	tickets  *ticket.MemoryStore
	events   *event.MemoryStore
	users    user.Store
	audit    *teamaudit.Memory
	dir      *identity.Memory
	weblab   identity.Group
	svc      ticket.Service
	ev       event.Event
	staff    authz.Principal
	guest    ticket.Ticket
	memberT  ticket.Ticket
	memberID uuid.UUID
}

func newWalkInDoor(t *testing.T, e event.Event) *walkInDoor {
	t.Helper()
	ctx := context.Background()
	d := &walkInDoor{tickets: ticket.NewMemoryStore(), events: event.NewMemoryStore(), users: user.NewMemoryStore(), audit: teamaudit.NewMemory()}
	d.dir = identity.NewMemory()
	d.weblab = identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"}
	d.dir.PutGroup(d.weblab)
	staff := uuid.New()
	d.staff = authz.Principal{ID: staff.String(), Groups: []string{"/UYELER/ARGE/GAMELAB"}}
	if e.Name == "" {
		e.Name, e.Location = "ARTLAB", "Tarihi Hamam"
	}
	e.OwnerTeam = "WEBLAB"
	e.DoorStaffIDs = []uuid.UUID{staff}
	if e.EntryMode == nil {
		e.EntryMode = entryMode(event.EntryVenue)
	}
	if e.EndDate == nil && e.StartDate == nil {
		end := time.Now().Add(8 * time.Hour)
		e.EndDate = &end
	}
	ev, err := d.events.Create(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if d.ev, err = d.events.Get(ctx, ev.ID); err != nil {
		t.Fatal(err)
	}
	if d.guest, err = d.tickets.Create(ctx, ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	d.memberID = uuid.New()
	if _, _, err := user.NewService(d.users).Ensure(ctx, d.memberID, user.Profile{Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper"}); err != nil {
		t.Fatal(err)
	}
	if d.memberT, err = d.tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &d.memberID}); err != nil {
		t.Fatal(err)
	}
	d.tickets.UseEvents(d.events)
	d.svc = ticket.NewService(mailingStore{d.tickets}, d.events, authz.NewAuthorizer(authz.DefaultPolicy()), d.users, d.dir, d.audit, log.New(io.Discard, "", 0))
	return d
}

// racingStore misses the guest Ticket of one e-mail at the first read, as a
// Walk-in does whose double submit wrote it just after.
type racingStore struct {
	mailingStore
	email  string
	missed bool
}

func (r *racingStore) GetByGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (ticket.Ticket, error) {
	if email == r.email && !r.missed {
		r.missed = true
		return ticket.Ticket{}, ticket.ErrNotFound
	}
	return r.MemoryStore.GetByGuestEvent(ctx, email, eventID)
}

// A Walk-in that lost a double submit (the Ticket written between its read
// and its write) answers existing, writes no second Ticket and closes its
// audit row failed, existing, without the other request's Ticket.
func TestWalkInDoubleSubmitIsNotRecordedAsItsWrite(t *testing.T) {
	t.Parallel()
	d := newWalkInDoor(t, event.Event{})
	svc := ticket.NewService(&racingStore{mailingStore: mailingStore{d.tickets}, email: "ada@example.com"}, d.events,
		authz.NewAuthorizer(authz.DefaultPolicy()), d.users, d.dir, d.audit, log.New(io.Discard, "", 0))
	before := d.count(t)
	out, err := svc.WalkIn(as(d.staff), d.staff, d.ev.ID, ticket.WalkIn{FirstName: "Ada", LastName: "L", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Result != ticket.WalkInExisting || out.TicketMail || out.Entry == nil || out.Entry.Source != ticket.EntryDesk {
		t.Fatalf("answer %+v %+v", out, out.Entry)
	}
	if d.count(t) != before {
		t.Fatal("a second Ticket was written")
	}
	rows := d.audit.Rows()
	if len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomeFailed || rows[0].ErrorCode != "existing" ||
		rows[0].After == nil || rows[0].After.TicketID != "" {
		t.Fatalf("audit rows %+v", rows)
	}
}

// as is ctx carrying p as the audit row's actor, as auditContext does.
func as(p authz.Principal) context.Context {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		id = uuid.New()
	}
	return teamaudit.WithActor(context.Background(), teamaudit.Actor{ID: id, Client: "core-frontend"})
}

func (d *walkInDoor) walkIn(p authz.Principal, w ticket.WalkIn) (ticket.WalkInOutcome, error) {
	return d.svc.WalkIn(as(p), p, d.ev.ID, w)
}

func (d *walkInDoor) count(t *testing.T) int {
	t.Helper()
	all, err := d.tickets.ListByEventStatus(context.Background(), d.ev.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

func (d *walkInDoor) today(t *testing.T) []ticket.DoorEntry {
	t.Helper()
	got, err := d.svc.ListEntries(context.Background(), d.staff, d.ev.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

var ayse = ticket.WalkIn{
	FirstName: " Ayşe ", LastName: "Yılmaz", Email: " Ayse@Example.com ",
	PhoneNumber: "0532 123 45 67", University: "Yıldız Teknik Üniversitesi", Department: "Bilgisayar Mühendisliği", Grade: "2",
}

// The door staff registers a person with no Ticket: a guest Ticket with what
// they gave (the phone in one form), its mail queued, the day's entry
// (walk_in) in the venue mode, and a ticket.walk_in audit row as door_staff
// through the Event's team.
func TestWalkInWritesTheGuestTicketAndTheDaysEntry(t *testing.T) {
	t.Parallel()
	d := newWalkInDoor(t, event.Event{})
	before := d.count(t)
	out, err := d.walkIn(d.staff, ayse)
	if err != nil {
		t.Fatal(err)
	}
	if out.Result != ticket.WalkInCreated || out.Name != "Ayşe Yılmaz" || out.TicketCode == "" || !out.TicketMail {
		t.Fatalf("answer %+v", out)
	}
	if out.Entry == nil || out.Entry.Source != ticket.EntryWalkIn || out.Entry.Name != "Ayşe Yılmaz" {
		t.Fatalf("entry %+v", out.Entry)
	}
	if d.count(t) != before+1 {
		t.Fatalf("%d Tickets, want %d", d.count(t), before+1)
	}
	got, err := d.tickets.GetByGuestEvent(context.Background(), "ayse@example.com", d.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TicketType != ticket.Guest || got.GuestFirstName != "Ayşe" || got.GuestEmail != "ayse@example.com" ||
		got.GuestPhoneNumber != "+905321234567" || got.GuestUniversity != "Yıldız Teknik Üniversitesi" ||
		got.GuestDepartment != "Bilgisayar Mühendisliği" || got.GuestGrade != "2" || !got.Active() {
		t.Fatalf("stored %+v", got)
	}
	entries := d.today(t)
	if len(entries) != 1 || entries[0].Source != ticket.EntryWalkIn {
		t.Fatalf("today's entries %+v", entries)
	}
	rows := d.audit.Rows()
	if len(rows) != 1 {
		t.Fatalf("%d audit rows", len(rows))
	}
	r := rows[0]
	if r.Action != teamaudit.ActionTicketWalkIn || r.Actor.Basis != teamaudit.BasisDoorStaff || r.Actor.ViaGroupID != d.weblab.ID ||
		r.GroupID != d.weblab.ID || r.Outcome != teamaudit.OutcomeOK || r.After == nil || r.After.TicketID != got.ID.String() ||
		r.After.EventID != d.ev.ID.String() {
		t.Fatalf("audit row %+v after %+v", r, r.After)
	}
}

// A person whose Ticket is already there (a guest's by its e-mail, a
// member's by their account e-mail) gets no second one: existing, no mail,
// no audit row, the day's entry as the desk's; a second time that day 409.
func TestWalkInFindsTheTicketAlreadyThere(t *testing.T) {
	t.Parallel()
	d := newWalkInDoor(t, event.Event{})
	before := d.count(t)
	for _, tc := range []struct {
		email, name string
	}{
		{"ADA@example.com", "Ada Lovelace"},
		{"grace@example.com", "Grace Hopper"},
	} {
		out, err := d.walkIn(d.staff, ticket.WalkIn{FirstName: "Başka", LastName: "Ad", Email: tc.email, PhoneNumber: "05321234567"})
		if err != nil {
			t.Fatalf("%s: %v", tc.email, err)
		}
		if out.Result != ticket.WalkInExisting || out.TicketMail || out.Name != tc.name || out.Entry == nil || out.Entry.Source != ticket.EntryDesk {
			t.Fatalf("%s: %+v %+v", tc.email, out, out.Entry)
		}
		_, err = d.walkIn(d.staff, ticket.WalkIn{FirstName: "Başka", LastName: "Ad", Email: tc.email})
		var entered *ticket.EntryConflictError
		if !errors.As(err, &entered) || entered.Name != tc.name {
			t.Fatalf("%s again: %v", tc.email, err)
		}
	}
	if d.count(t) != before {
		t.Fatalf("%d Tickets, want %d", d.count(t), before)
	}
	guest, _ := d.tickets.Get(context.Background(), d.guest.ID)
	if guest.GuestFirstName != "Ada" || guest.GuestPhoneNumber != "" {
		t.Fatalf("the Ticket found was changed: %+v", guest)
	}
	if rows := d.audit.Rows(); len(rows) != 0 {
		t.Fatalf("%d audit rows", len(rows))
	}
}

// Only who may write the Event's door: an ordinary member is refused and
// nothing is written; the Owner team's Leader and YK may, recorded as such.
func TestWalkInRights(t *testing.T) {
	t.Parallel()
	d := newWalkInDoor(t, event.Event{})
	before := d.count(t)
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/GAMELAB"}}
	if _, err := d.walkIn(member, ayse); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("a member: %v", err)
	}
	if d.count(t) != before || len(d.audit.Rows()) != 0 {
		t.Fatal("a refused Walk-in wrote")
	}
	lead := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	if _, err := d.walkIn(lead, ticket.WalkIn{FirstName: "Lider", LastName: "Kişi", Email: "lider@example.com"}); err != nil {
		t.Fatalf("the Leader: %v", err)
	}
	if _, err := d.walkIn(yk, ticket.WalkIn{FirstName: "YK", LastName: "Kişi", Email: "yk@example.com"}); err != nil {
		t.Fatalf("YK: %v", err)
	}
	rows := d.audit.Rows()
	if len(rows) != 2 || rows[0].Actor.Basis != teamaudit.BasisLeader || rows[0].Actor.ViaGroupID != d.weblab.ID ||
		rows[1].Actor.Basis != teamaudit.BasisPrivileged {
		t.Fatalf("rows %+v", rows)
	}
}

// Without the caller in ctx the audit row cannot be opened: 503 before
// anything is written (the handler passes auditContext).
func TestWalkInWithoutTheActorWritesNothing(t *testing.T) {
	t.Parallel()
	d := newWalkInDoor(t, event.Event{})
	before := d.count(t)
	if _, err := d.svc.WalkIn(context.Background(), d.staff, d.ev.ID, ayse); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("no actor: %v", err)
	}
	d.audit.SetUnavailable(true)
	if _, err := d.walkIn(d.staff, ayse); err == nil {
		t.Fatal("a Walk-in without its audit row")
	}
	if d.count(t) != before || len(d.today(t)) != 0 {
		t.Fatal("written without its audit row")
	}
}

// A full Event refuses a Walk-in, never its waiting list; the audit row says
// why.
func TestWalkInOnAFullEventIsRefused(t *testing.T) {
	t.Parallel()
	d := newWalkInDoor(t, event.Event{Capacity: 2})
	before := d.count(t)
	_, err := d.walkIn(d.staff, ayse)
	var full *ticket.EventFullError
	if !errors.As(err, &full) || !full.WaitlistEnabled {
		t.Fatalf("full: %v", err)
	}
	if d.count(t) != before {
		t.Fatal("a Ticket was written on a full Event")
	}
	rows := d.audit.Rows()
	if len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomeFailed || rows[0].ErrorCode != "event_full" {
		t.Fatalf("rows %+v", rows)
	}
}

// A draft, an Event that has ended: refused with the reason.
func TestWalkInOnAClosedEvent(t *testing.T) {
	t.Parallel()
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	draft := newWalkInDoor(t, event.Event{Status: event.StatusDraft})
	var closed *ticket.EventClosedError
	if _, err := draft.walkIn(yk, ayse); !errors.As(err, &closed) || closed.Reason != "event_not_published" {
		t.Fatalf("draft: %v", err)
	}
	start, end := time.Now().Add(-5*time.Hour), time.Now().Add(-time.Hour)
	ended := newWalkInDoor(t, event.Event{StartDate: &start, EndDate: &end})
	if _, err := ended.walkIn(ended.staff, ayse); !errors.As(err, &closed) || closed.Reason != "event_ended" {
		t.Fatalf("ended: %v", err)
	}
}

// per_session: the Ticket alone, no entry.
func TestWalkInPerSessionWritesNoEntry(t *testing.T) {
	t.Parallel()
	d := newWalkInDoor(t, event.Event{EntryMode: entryMode(event.EntryPerSession)})
	// per_session holds only while the Event has a live Session.
	seedSession(t, d.events, seedDay(t, d.events, d.ev.ID, "Gün 1").ID, "Açılış")
	out, err := d.walkIn(d.staff, ayse)
	if err != nil {
		t.Fatal(err)
	}
	if out.Result != ticket.WalkInCreated || out.Entry != nil || !out.TicketMail {
		t.Fatalf("%+v", out)
	}
}

// A waitlisted Ticket and one an operator cancelled are refused; one its
// holder cancelled is reopened.
func TestWalkInTicketStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newWalkInDoor(t, event.Event{})
	if _, _, err := d.tickets.Cancel(ctx, d.guest.ID, ticket.CancelledByOperator, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.walkIn(d.staff, ticket.WalkIn{FirstName: "Ada", LastName: "L", Email: "ada@example.com"}); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Fatalf("operator cancelled: %v", err)
	}
	if _, _, err := d.tickets.Cancel(ctx, d.memberT.ID, ticket.CancelledByHolder, ""); err != nil {
		t.Fatal(err)
	}
	out, err := d.walkIn(d.staff, ticket.WalkIn{FirstName: "Grace", LastName: "H", Email: "grace@example.com"})
	if err != nil {
		t.Fatalf("holder cancelled: %v", err)
	}
	if out.Result != ticket.WalkInCreated || out.Name != "Grace Hopper" || out.Entry == nil || out.Entry.Source != ticket.EntryWalkIn {
		t.Fatalf("reopened %+v", out)
	}
	if got, _ := d.tickets.Get(ctx, d.memberT.ID); !got.Active() {
		t.Fatalf("not reopened: %+v", got)
	}
}

// The fields: required, the e-mail's shape, the phone in one form, the
// grade's keys, the lengths. Nothing is written for a refused one.
func TestWalkInFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		edit  func(*ticket.WalkIn)
		code  string
		phone string
	}{
		{"no name", func(w *ticket.WalkIn) { w.FirstName = " " }, ticket.CodeGuestFieldsRequired, ""},
		{"no e-mail", func(w *ticket.WalkIn) { w.Email = "" }, ticket.CodeGuestFieldsRequired, ""},
		{"e-mail without a domain", func(w *ticket.WalkIn) { w.Email = "ayse@example" }, ticket.CodeEmailInvalid, ""},
		{"e-mail with a space", func(w *ticket.WalkIn) { w.Email = "ay se@example.com" }, ticket.CodeEmailInvalid, ""},
		{"two @", func(w *ticket.WalkIn) { w.Email = "a@b@example.com" }, ticket.CodeEmailInvalid, ""},
		{"phone too short", func(w *ticket.WalkIn) { w.PhoneNumber = "0532 123" }, ticket.CodePhoneInvalid, ""},
		{"phone with letters", func(w *ticket.WalkIn) { w.PhoneNumber = "0532abc4567" }, ticket.CodePhoneInvalid, ""},
		{"grade", func(w *ticket.WalkIn) { w.Grade = "5" }, ticket.CodeGradeInvalid, ""},
		{"university", func(w *ticket.WalkIn) { w.University = longText(201) }, ticket.CodeUniversityTooLong, ""},
		{"department", func(w *ticket.WalkIn) { w.Department = longText(201) }, ticket.CodeDepartmentTooLong, ""},
		{"first name", func(w *ticket.WalkIn) { w.FirstName = longText(101) }, ticket.CodeGuestNameTooLong, ""},
		{"a line break in the name", func(w *ticket.WalkIn) { w.LastName = "Yıl\nmaz" }, ticket.CodeGuestFieldInvalid, ""},
		{"a tab in the department", func(w *ticket.WalkIn) { w.Department = "Bilgisayar\tMüh." }, ticket.CodeGuestFieldInvalid, ""},
		{"a NUL in the university", func(w *ticket.WalkIn) { w.University = "YTÜ\x00" }, ticket.CodeGuestFieldInvalid, ""},
		{"+90", func(w *ticket.WalkIn) { w.PhoneNumber = "+90 (532) 123-45-67" }, "", "+905321234567"},
		{"90", func(w *ticket.WalkIn) { w.PhoneNumber = "905321234567" }, "", "+905321234567"},
		{"ten digits", func(w *ticket.WalkIn) { w.PhoneNumber = "532.123.45.67" }, "", "+905321234567"},
		{"landline", func(w *ticket.WalkIn) { w.PhoneNumber = "0212 383 00 00" }, "", "+902123830000"},
		{"abroad", func(w *ticket.WalkIn) { w.PhoneNumber = "+49 151 2345 6789" }, "", "+4915123456789"},
		{"abroad 00", func(w *ticket.WalkIn) { w.PhoneNumber = "0049 151 23456789" }, "", "+4915123456789"},
		{"no phone", func(w *ticket.WalkIn) { w.PhoneNumber = " " }, "", ""},
	} {
		d := newWalkInDoor(t, event.Event{})
		w := ayse
		w.University, w.Department = "", ""
		tc.edit(&w)
		before := d.count(t)
		_, err := d.walkIn(d.staff, w)
		if tc.code != "" {
			var refused *ticket.InvalidError
			if !errors.As(err, &refused) || refused.Code != tc.code {
				t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
			}
			if d.count(t) != before {
				t.Errorf("%s: written", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got, err := d.tickets.GetByGuestEvent(context.Background(), "ayse@example.com", d.ev.ID)
		if err != nil || got.GuestPhoneNumber != tc.phone {
			t.Errorf("%s: phone %q %v, want %q", tc.name, got.GuestPhoneNumber, err, tc.phone)
		}
	}
}

func longText(n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = 'a'
	}
	return string(out)
}
