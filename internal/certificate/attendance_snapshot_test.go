package certificate_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// programSession is a Session of a snapshot fixture's program day.
type programSession struct {
	title      string
	counted    bool
	start, end *time.Time
}

// newProgramFixture is the attendance fixture with its own Sessions unmarked
// (N counts only the program day's) and a program day of the given
// Sessions, all yesterday.
func newProgramFixture(t *testing.T, sessions []programSession) attendanceFixture {
	t.Helper()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	for title, sess := range f.sessions {
		sess.CertificateCounted, sess.CertificateRequired = false, false
		if _, err := f.events.UpdateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
		delete(f.sessions, title)
	}
	day, err := f.events.CreateDay(ctx, event.Day{EventID: f.ev.ID, Name: "Program"})
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range sessions {
		sess, err := f.events.CreateSession(ctx, event.Session{
			EventDayID: day.ID, Title: s.title, SpeakerName: "Ada", SessionType: "PRESENTATION", OrderIndex: i,
			CertificateCounted: s.counted, StartTime: s.start, EndTime: s.end,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.sessions[s.title] = sess
	}
	return f
}

// eightTalks are eight counted 90-minute Sessions, Talk 1 to Talk 8, and a
// NETWORKING one (Ağ) that is not counted.
func eightTalks() []programSession {
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Hour)
	out := make([]programSession, 0, 9)
	for i := 0; i < 8; i++ {
		start := base.Add(time.Duration(i) * 2 * time.Hour)
		end := start.Add(90 * time.Minute)
		out = append(out, programSession{title: "Talk " + strconv.Itoa(i+1), counted: true, start: &start, end: &end})
	}
	start := base.Add(20 * time.Hour)
	end := start.Add(3 * time.Hour)
	return append(out, programSession{title: "Ağ", counted: false, start: &start, end: &end})
}

// certificatesOf is a Ticket's active certificate and its revoked ones.
func certificatesOf(t *testing.T, f attendanceFixture, ticketID uuid.UUID) (active certificate.Certificate, revoked []certificate.Certificate) {
	t.Helper()
	listed, err := f.svc.ListByEvent(context.Background(), f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range listed {
		if c.TicketID != ticketID {
			continue
		}
		if c.RevokedAt == nil {
			active = c
		} else {
			revoked = append(revoked, c)
		}
	}
	return active, revoked
}

// checkIn checks the Ticket in on the Session.
func (f attendanceFixture) checkIn(t *testing.T, ticketID uuid.UUID, sess event.Session) {
	t.Helper()
	if _, err := f.tickets.AddCheckIn(context.Background(), ticket.CheckIn{ID: uuid.New(), TicketID: ticketID, EventDayID: sess.EventDayID, SessionID: sess.ID}); err != nil {
		t.Fatal(err)
	}
}

func processAll(t *testing.T, f attendanceFixture) {
	t.Helper()
	if _, err := f.svc.ProcessNext(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
}

func wantSnapshot(t *testing.T, label string, got *certificate.AttendanceSnapshot, attended, total, minutes int) {
	t.Helper()
	if got == nil || got.Attended != attended || got.CountedTotal != total || got.ProgramMinutes != minutes {
		t.Fatalf("%s snapshot %+v, want %d/%d/%d", label, got, attended, total, minutes)
	}
}

// A certificate keeps the attendance it was issued on: 6 of 8 counted
// 90-minute Sessions is 6/8/540, the NETWORKING check-in counts neither in
// the share nor in the minutes. One given below the rule by hand carries its
// real count. A reissue takes the attendance anew and leaves the old row's
// as it was. The public verification shows it with no Session, e-mail,
// User id, Ticket id or justification; a certificate issued before has none.
func TestAttendanceSnapshotAtIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProgramFixture(t, eightTalks())
	ayse := f.member(t, "Ayşe", "Yılmaz", "Talk 1", "Talk 2", "Talk 3", "Talk 4", "Talk 5", "Talk 6", "Ağ")
	burak := f.member(t, "Burak", "Kaya", "Talk 1", "Talk 2", "Talk 3", "Talk 4", "Ağ")

	batch, err := f.svc.Finalize(ctx, f.leader, f.ev.ID)
	if err != nil || batch.TotalCount != 1 {
		t.Fatalf("finalize %v %+v", err, batch)
	}
	processAll(t, f)
	if _, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, burak, "Raporlu, iki oturumu kaçırdı"); err != nil {
		t.Fatal(err)
	}
	processAll(t, f)

	first, _ := certificatesOf(t, f, ayse)
	wantSnapshot(t, "eligible", first.Attendance, 6, 8, 540)
	below, _ := certificatesOf(t, f, burak)
	wantSnapshot(t, "manual below the rule", below.Attendance, 4, 8, 360)

	public, err := f.svc.Verify(ctx, first.Serial)
	if err != nil {
		t.Fatal(err)
	}
	if public.Attendance == nil || *public.Attendance != (certificate.PublicAttendance{Attended: 6, Total: 8, ProgramMinutes: 540}) {
		t.Fatalf("public attendance %+v", public.Attendance)
	}
	raw, _ := json.Marshal(public)
	if !strings.Contains(string(raw), `"attendance":{"attended":6,"total":8,"programMinutes":540}`) {
		t.Fatalf("public json %s", raw)
	}
	manual, err := f.svc.Verify(ctx, below.Serial)
	if err != nil {
		t.Fatal(err)
	}
	rawManual, _ := json.Marshal(manual)
	for _, body := range []string{string(raw), string(rawManual)} {
		for _, forbidden := range []string{"@example.com", "ticketId", "ownerId", "userId", "Talk", "Ağ", "Raporlu", "justification", "manual", "checkIn"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("public response leaks %q: %s", forbidden, body)
			}
		}
	}

	// Ayşe attends a seventh Session; the reissue takes 7/8/630.
	sess := f.sessions["Talk 7"]
	f.checkIn(t, ayse, sess)
	if _, err := f.svc.Reissue(ctx, f.leader, first.Serial); err != nil {
		t.Fatal(err)
	}
	processAll(t, f)
	second, revoked := certificatesOf(t, f, ayse)
	if second.Serial == first.Serial || len(revoked) != 1 || revoked[0].Serial != first.Serial {
		t.Fatalf("reissue %+v %+v", second, revoked)
	}
	wantSnapshot(t, "reissued", second.Attendance, 7, 8, 630)
	wantSnapshot(t, "replaced", revoked[0].Attendance, 6, 8, 540)
	old, err := f.svc.Verify(ctx, first.Serial)
	if err != nil || old.Status != "revoked" || old.Attendance == nil || old.Attendance.Attended != 6 {
		t.Fatalf("revoked verification %v %+v", err, old)
	}

	// A certificate issued before core kept the attendance shows none.
	before, err := f.store.Create(ctx, certificate.Certificate{
		EventID: f.ev.ID, TicketID: uuid.New(), Serial: strings.Repeat("A", 32), RecipientName: "Cem Demir",
		RecipientEmail: "cem@example.com", EventName: f.ev.Name, OwnerTeam: "ARTLAB", VerifyURL: "https://skyl.app/c/" + strings.Repeat("A", 32),
	}, []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := f.svc.Verify(ctx, before.Serial)
	if err != nil || legacy.Attendance != nil {
		t.Fatalf("certificate issued before: %v %+v", err, legacy)
	}
	if rawLegacy, _ := json.Marshal(legacy); strings.Contains(string(rawLegacy), "attendance") {
		t.Fatalf("certificate issued before has attendance: %s", rawLegacy)
	}
}

// A Session without its times adds 0 minutes, one that ends before it
// starts adds 0; both still count in the share.
func TestAttendanceSnapshotProgramMinutesEdges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Now().Add(-24 * time.Hour).Truncate(time.Minute)
	end := start.Add(45 * time.Minute)
	backwards := start.Add(-30 * time.Minute)
	onlyStart := start.Add(2 * time.Hour)
	f := newProgramFixture(t, []programSession{
		{title: "Kısa", counted: true, start: &start, end: &end},
		{title: "Saatsiz", counted: true},
		{title: "Ters", counted: true, start: &start, end: &backwards},
		{title: "Yarım", counted: true, start: &onlyStart},
	})
	id := f.member(t, "Ayşe", "Yılmaz", "Kısa", "Saatsiz", "Ters", "Yarım")
	if _, err := f.svc.Finalize(ctx, f.leader, f.ev.ID); err != nil {
		t.Fatal(err)
	}
	processAll(t, f)
	got, _ := certificatesOf(t, f, id)
	wantSnapshot(t, "edges", got.Attendance, 4, 4, 45)
}
