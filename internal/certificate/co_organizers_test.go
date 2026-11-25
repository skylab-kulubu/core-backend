package certificate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// WEBLAB co-organizes ARTLAB's Event: WEBLAB's Leader reads its
// certificate workspace and finalizes its attendance as ARTLAB's Leader
// does; a WEBLAB member with certificate:issue does nothing in a "leaders"
// team, and does as a Certificate manager in a "members" one
// (docs/co-organizers.md).
func TestCoOrganizersRunTheEventsCertificates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	f.member(t, "Ayşe", "Yılmaz", "Açılış", "Talk 1", "Talk 2")
	write := func(management event.Management) {
		t.Helper()
		if _, err := f.events.SetCoOrganizers(ctx, event.CoOrganizersWrite{
			EventID: f.ev.ID, OwnerTeam: "ARTLAB",
			Teams: []event.CoOrganizerTeam{{Code: "WEBLAB", Management: management, Visibility: event.VisibilityCoOrganizer}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(event.ManagementLeaders)
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if _, err := f.svc.Attendance(ctx, leader, f.ev.ID); err != nil {
		t.Fatalf("WEBLAB's Leader reads the attendance: %v", err)
	}
	manager := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}, Roles: []string{"certificate:issue"}}
	if _, err := f.svc.Attendance(ctx, manager, f.ev.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("a leaders team's member with certificate:issue reads: %v", err)
	}
	if _, err := f.svc.Finalize(ctx, manager, f.ev.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("a leaders team's member with certificate:issue finalizes: %v", err)
	}
	write(event.ManagementMembers)
	if _, err := f.svc.Attendance(ctx, manager, f.ev.ID); err != nil {
		t.Fatalf("a members team's Certificate manager reads: %v", err)
	}
	if _, err := f.svc.Finalize(ctx, leader, f.ev.ID); errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("WEBLAB's Leader finalizes: %v", err)
	}
}
