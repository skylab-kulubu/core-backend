package event_test

import (
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/event"
)

// registrationCase is one row of the Registration closure table (ticket
// 12): the status and dates (as in statusCases) against closing by hand and
// a deadline gone or to come.
type registrationCase struct {
	name     string
	status   event.Status
	ended    bool
	closed   bool
	deadline string // "", "past" or "future"
	open     bool   // RegistrationOpenAt
	closedAt bool   // RegistrationClosedAt
}

func registrationCases() []registrationCase {
	var cases []registrationCase
	for _, status := range []event.Status{event.StatusDraft, event.StatusPublished, event.StatusCancelled} {
		for _, ended := range []bool{false, true} {
			for _, closed := range []bool{false, true} {
				for _, deadline := range []string{"", "past", "future"} {
					closedAt := closed || deadline == "past"
					cases = append(cases, registrationCase{
						name:   string(status) + map[bool]string{false: " running", true: " ended"}[ended] + map[bool]string{false: "", true: " closed"}[closed] + " deadline=" + deadline,
						status: status, ended: ended, closed: closed, deadline: deadline,
						open:     status == event.StatusPublished && !ended && !closedAt,
						closedAt: closedAt,
					})
				}
			}
		}
	}
	return cases
}

func (tc registrationCase) event(now time.Time) event.Event {
	start := now.Add(48 * time.Hour)
	if tc.ended {
		start = now.Add(-72 * time.Hour)
	}
	e := event.Event{Name: tc.name, Location: "YTÜ", OwnerTeam: "WEBLAB", Status: tc.status, StartDate: &start, RegistrationClosed: tc.closed}
	switch tc.deadline {
	case "past":
		e.RegistrationClosesAt = at(now.Add(-time.Hour))
	case "future":
		e.RegistrationClosesAt = at(now.Add(time.Hour))
	}
	return e
}

func TestRegistrationOpenIsDerived(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range registrationCases() {
		e := tc.event(now)
		if got := e.RegistrationOpenAt(now); got != tc.open {
			t.Errorf("%s: open = %v, want %v", tc.name, got, tc.open)
		}
		if got := e.ApplyingClosedAt(now); got != tc.closedAt {
			t.Errorf("%s: closed = %v, want %v", tc.name, got, tc.closedAt)
		}
	}
	// The deadline itself is closed.
	e := event.Event{Status: event.StatusPublished, RegistrationClosesAt: at(now)}
	if e.RegistrationOpenAt(now) || !e.ApplyingClosedAt(now) {
		t.Fatal("open at the deadline")
	}
}

// At an answer's sending, applying closed by hand counts from when it was
// closed; with no time known, from ever.
func TestApplyingClosedAtAnAnswersSending(t *testing.T) {
	t.Parallel()
	closedAt := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	e := event.Event{Status: event.StatusPublished, RegistrationClosed: true, RegistrationClosedAt: &closedAt}
	if e.ApplyingClosedAt(closedAt.Add(-time.Second)) || !e.ApplyingClosedAt(closedAt) || !e.ApplyingClosedAt(closedAt.Add(time.Hour)) {
		t.Fatal("by hand")
	}
	e.RegistrationClosedAt = nil
	if !e.ApplyingClosedAt(closedAt.Add(-time.Hour)) {
		t.Fatal("closed by hand at an unknown time")
	}
	deadline := closedAt
	e = event.Event{Status: event.StatusPublished, RegistrationClosesAt: &deadline}
	if e.ApplyingClosedAt(deadline.Add(-time.Second)) || !e.ApplyingClosedAt(deadline) {
		t.Fatal("deadline")
	}
}
