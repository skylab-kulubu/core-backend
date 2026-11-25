package certificate_test

import (
	"context"
	"errors"
	"strings"
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

// A Co-organizer's rights are the Event's own: WEBLAB's Leader, a
// Co-organizer of one ARTLAB Event, does nothing on another ARTLAB Event
// (docs/co-organizers.md).
func TestCoOrganizerRightsStayOnTheirEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	if _, err := f.events.SetCoOrganizers(ctx, event.CoOrganizersWrite{
		EventID: f.ev.ID, OwnerTeam: "ARTLAB",
		Teams: []event.CoOrganizerTeam{{Code: "WEBLAB", Management: event.ManagementLeaders, Visibility: event.VisibilityCoOrganizer}},
	}); err != nil {
		t.Fatal(err)
	}
	other, err := f.events.Create(ctx, event.Event{Name: "ARTLAB 2027", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleCount})
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if _, err := f.svc.Attendance(ctx, leader, other.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("the attendance of an Event WEBLAB does not co-organize: %v", err)
	}
	if _, err := f.svc.Finalize(ctx, leader, other.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("finalizing an Event WEBLAB does not co-organize: %v", err)
	}
}

// A certificate is revoked by its Event's organizers as they are now, not
// by the Owner team it was issued under: the Event moved from ARTLAB to
// GAMELAB, ARTLAB's Leader no longer revokes it and GAMELAB's does
// (docs/co-organizers.md).
func TestRevokeFollowsTheEventsOrganizers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	issue := func() string {
		t.Helper()
		serial := strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := f.store.Create(ctx, certificate.Certificate{
			EventID: f.ev.ID, TicketID: uuid.New(), Serial: serial, RecipientName: "Cem Demir",
			RecipientEmail: "cem@example.com", EventName: f.ev.Name, OwnerTeam: "ARTLAB", VerifyURL: "https://skyl.app/c/" + serial,
		}, []byte("%PDF")); err != nil {
			t.Fatal(err)
		}
		return serial
	}
	first, second := issue(), issue()
	moved := f.ev
	moved.OwnerTeam = "GAMELAB"
	if _, err := f.events.Update(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Revoke(ctx, f.leader, first); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("ARTLAB's Leader revokes after the move: %v", err)
	}
	gamelab := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/GAMELAB/LIDERLER"}}
	if err := f.svc.Revoke(ctx, gamelab, second); err != nil {
		t.Fatalf("GAMELAB's Leader revokes: %v", err)
	}
}
