package certificate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The Postgres store reads the program minutes as the memory one does (a
// Session without both times or ending before it starts adds 0, a
// non-counted one adds nothing, a second check-in on a Session adds nothing),
// and keeps a certificate's snapshot through Create, Replace and every read;
// a certificate issued before reads back without one.
func TestPostgresAttendanceSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := certificate.NewPostgresStore(pool)
	ev, sessions, dayOf := pgProgram(t, ctx, pool, "Program")
	// S1–S3 last 90 minutes; S5 has no times; S6 ends before it starts;
	// S4 (not counted) lasts 3 hours.
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`UPDATE sessions SET start_time = now() - interval '2 days', end_time = now() - interval '2 days' + interval '90 minutes' WHERE id = ANY($1)`, []uuid.UUID{sessions[1], sessions[2], sessions[3]}},
		{`UPDATE sessions SET start_time = now() - interval '2 days', end_time = now() - interval '2 days' - interval '30 minutes' WHERE id = $1`, sessions[6]},
		{`UPDATE sessions SET start_time = now() - interval '2 days', end_time = now() - interval '2 days' + interval '3 hours' WHERE id = $1`, sessions[4]},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.arg); err != nil {
			t.Fatal(err)
		}
	}
	ticketID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ayşe', 'Yılmaz', 'ayse@example.com')`, ticketID, ev.ID); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 2, 3, 3, 4, 5, 6} {
		if _, err := pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, uuid.New(), ticketID, dayOf[i], sessions[i]); err != nil {
			t.Fatal(err)
		}
	}
	attendance, err := store.EventAttendance(ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attendance.Tickets) != 1 || attendance.Tickets[0].Attended != 5 || attendance.Tickets[0].ProgramMinutes != 270 {
		t.Fatalf("attendance %+v", attendance.Tickets)
	}

	snapshot := &certificate.AttendanceSnapshot{Attended: 5, CountedTotal: 8, ProgramMinutes: 270}
	created, err := store.Create(ctx, certificate.Certificate{
		EventID: ev.ID, TicketID: ticketID, Serial: "ILK", RecipientName: "Ayşe Yılmaz", RecipientEmail: "ayse@example.com",
		EventName: ev.Name, OwnerTeam: "ARTLAB", Attendance: snapshot,
	}, []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Attendance == nil || *created.Attendance != *snapshot {
		t.Fatalf("created %+v", created.Attendance)
	}
	replaced, err := store.Replace(ctx, "ILK", certificate.Certificate{
		EventID: ev.ID, TicketID: ticketID, Serial: "YENI", RecipientName: "Ayşe Yılmaz", RecipientEmail: "ayse@example.com",
		EventName: ev.Name, OwnerTeam: "ARTLAB", Attendance: &certificate.AttendanceSnapshot{Attended: 6, CountedTotal: 8, ProgramMinutes: 360},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Attendance == nil || replaced.Attendance.Attended != 6 {
		t.Fatalf("replaced %+v", replaced.Attendance)
	}
	first, _, err := store.GetBySerial(ctx, "ILK")
	if err != nil || first.RevokedAt == nil || first.Attendance == nil || *first.Attendance != *snapshot {
		t.Fatalf("first after replace %v %+v", err, first)
	}
	active, err := store.GetActive(ctx, ev.ID, ticketID)
	if err != nil || active.Serial != "YENI" || active.Attendance == nil || active.Attendance.ProgramMinutes != 360 {
		t.Fatalf("active %v %+v", err, active)
	}

	// Issued before core kept the snapshot.
	otherTicket := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Cem', 'Demir', 'cem@example.com')`, otherTicket, ev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, certificate.Certificate{
		EventID: ev.ID, TicketID: otherTicket, Serial: "ESKI", RecipientName: "Cem Demir", RecipientEmail: "cem@example.com",
		EventName: ev.Name, OwnerTeam: "ARTLAB",
	}, nil); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListByEvent(ctx, ev.ID)
	if err != nil || len(listed) != 3 {
		t.Fatalf("listed %v %d", err, len(listed))
	}
	for _, c := range listed {
		if (c.Serial == "ESKI") != (c.Attendance == nil) {
			t.Fatalf("%s snapshot %+v", c.Serial, c.Attendance)
		}
	}
	revoked, err := store.Revoke(ctx, "ESKI")
	if err != nil || revoked.Attendance != nil {
		t.Fatalf("revoked %v %+v", err, revoked.Attendance)
	}
}
