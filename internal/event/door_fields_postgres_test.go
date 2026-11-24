package event_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The external registration link, the two door mail switches and the
// attendee note sit next to each other in the insert and the update; each
// lands in its own column on create and on update.
func TestPostgresWritesTheDoorFieldsTogether(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	off, on := false, true
	note := "Wi-Fi: SKYLAB"
	created, err := svc.Create(ctx, yk, event.Event{
		Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: stamp("2031-03-14T10:00:00Z"),
		ExternalRegistrationURL: "https://forms.office.com/r/AbC",
		AttendanceWelcomeMail:   &off, AttendanceRecordedMail: &on, AttendeeNote: &note,
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(when, url string, welcome, recorded bool, note string) {
		t.Helper()
		got, err := store.Get(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExternalRegistrationURL != url ||
			got.AttendanceWelcomeMail == nil || *got.AttendanceWelcomeMail != welcome ||
			got.AttendanceRecordedMail == nil || *got.AttendanceRecordedMail != recorded ||
			got.AttendeeNote == nil || *got.AttendeeNote != note {
			t.Fatalf("%s: url %q welcome %v recorded %v note %v", when, got.ExternalRegistrationURL, got.AttendanceWelcomeMail, got.AttendanceRecordedMail, got.AttendeeNote)
		}
	}
	check("create", "https://forms.office.com/r/AbC", false, true, "Wi-Fi: SKYLAB")

	if _, err := svc.Patch(ctx, yk, created.ID, patchOf(t, `{"externalRegistrationUrl":"https://lu.ma/x","attendanceWelcomeMail":true,"attendanceRecordedMail":false,"attendeeNote":"Salon D-001"}`)); err != nil {
		t.Fatal(err)
	}
	check("update", "https://lu.ma/x", true, false, "Salon D-001")

	if _, err := svc.Patch(ctx, yk, created.ID, patchOf(t, `{"name":"Gecekodu 2"}`)); err != nil {
		t.Fatal(err)
	}
	check("update without them", "https://lu.ma/x", true, false, "Salon D-001")
}
