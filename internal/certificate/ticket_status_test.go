package certificate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// On the memory stores too, a cancelled Ticket is off the attendance list
// and gets no certificate by hand, with a justification neither
// (docs/ticket-cancellation.md).
func TestCancelledTicketIsOffTheAttendance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	kept := f.member(t, "Ayşe", "Yılmaz", "Açılış", "Talk 1", "Talk 2")
	gone := f.member(t, "Burak", "Kaya")
	if _, _, err := f.tickets.Cancel(ctx, gone, ticket.CancelledByOperator, ""); err != nil {
		t.Fatal(err)
	}
	list, err := f.svc.Attendance(ctx, f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows := rowsByTicket(list)
	if _, ok := rows[gone]; ok || len(rows) != 1 {
		t.Fatalf("rows %+v", list.Rows)
	}
	if _, ok := rows[kept]; !ok {
		t.Fatalf("the active Ticket is missing: %+v", list.Rows)
	}
	if _, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, gone, "Gerekçeli"); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("by hand: %v", err)
	}
	if _, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, kept, ""); err != nil {
		t.Fatalf("the active Ticket: %v", err)
	}
}
