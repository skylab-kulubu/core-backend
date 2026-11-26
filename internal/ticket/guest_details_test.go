package ticket_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// A form answer's guest details (docs/form-mappings.md#the-details): a new
// guest Ticket takes them; one already there takes only what it lacks, as
// its phone number; a detail the columns would refuse is left out.
func TestAFormAnswerWritesTheGuestsDetails(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))

	report := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")
	report.Guest.PhoneNumber, report.Guest.University, report.Guest.Department, report.Guest.Grade = "+905551112233", "YTÜ", "Fizik", "3"
	if _, err := svc.RecordFormResponse(ctx, formsService, report); err != nil {
		t.Fatal(err)
	}
	got := storedGuest(t, svc, ev, "ada@example.com")
	if got.GuestPhoneNumber != "+905551112233" || got.GuestUniversity != "YTÜ" || got.GuestDepartment != "Fizik" || got.GuestGrade != "3" {
		t.Fatalf("created %+v", got)
	}

	// Already there with a phone number and nothing else: the answer fills
	// the rest, the phone stays.
	if _, err := svc.ApplyGuest(ctx, yk, ev.ID, ticket.GuestInfo{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com", PhoneNumber: "+905550000000"}); err != nil {
		t.Fatal(err)
	}
	report = acceptedGuest(formID, "Grace", "Hopper", "grace@example.com")
	report.Guest.PhoneNumber, report.Guest.University, report.Guest.Grade = "+905559999999", "İTÜ", "masters"
	if _, err := svc.RecordFormResponse(ctx, formsService, report); err != nil {
		t.Fatal(err)
	}
	got = storedGuest(t, svc, ev, "grace@example.com")
	if got.GuestPhoneNumber != "+905550000000" || got.GuestUniversity != "İTÜ" || got.GuestDepartment != "" || got.GuestGrade != "masters" {
		t.Fatalf("filled %+v", got)
	}

	report = acceptedGuest(formID, "Linus", "Torvalds", "linus@example.com")
	report.Guest.University, report.Guest.Department, report.Guest.Grade = strings.Repeat("ü", ticket.MaxWalkInText+1), "Kimya", "5. sınıf"
	if _, err := svc.RecordFormResponse(ctx, formsService, report); err != nil {
		t.Fatal(err)
	}
	got = storedGuest(t, svc, ev, "linus@example.com")
	if got.GuestUniversity != "" || got.GuestDepartment != "Kimya" || got.GuestGrade != "" {
		t.Fatalf("refused details %+v", got)
	}
}

func TestNormalizePhone(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{"0555 111 22 33": "+905551112233", "+44 20 7946 0958": "+442079460958", "yok": ""} {
		got, ok := ticket.NormalizePhone(raw)
		if got != want || ok != (want != "") {
			t.Errorf("NormalizePhone(%q) = %q %t", raw, got, ok)
		}
	}
}
