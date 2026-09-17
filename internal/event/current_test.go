package event_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

func talk(title string, start, end time.Time) event.Session {
	s, e := start, end
	return event.Session{
		ID: uuid.New(), Title: title, SpeakerName: "Ada", SessionType: "PRESENTATION",
		StartTime: &s, EndTime: &e,
	}
}

func TestResolveCurrent_ClockMatch(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	first := talk("One", start, start.Add(time.Hour))
	second := talk("Two", start.Add(2*time.Hour), start.Add(3*time.Hour))
	got := event.ResolveCurrent([]event.Session{first, second}, start.Add(30*time.Minute))
	if !got.ClockMatch || got.Session == nil || got.Session.Title != "One" {
		t.Fatalf("got %+v", got)
	}
	if len(got.Candidates) != 1 {
		t.Fatalf("candidates %+v", got.Candidates)
	}
}

func TestResolveCurrent_OverlapIsPick(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	roomA := talk("A", start, start.Add(2*time.Hour))
	roomB := talk("B", start.Add(30*time.Minute), start.Add(90*time.Minute))
	got := event.ResolveCurrent([]event.Session{roomA, roomB}, start.Add(time.Hour))
	if got.ClockMatch || got.Session != nil || len(got.Candidates) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveCurrent_OutsideScheduleOffersAll(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	sess := talk("One", start, start.Add(time.Hour))
	got := event.ResolveCurrent([]event.Session{sess}, start.Add(5*time.Hour))
	if got.ClockMatch || got.Session != nil || len(got.Candidates) != 1 || got.Candidates[0].Title != "One" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveCurrent_MissingTimesArePick(t *testing.T) {
	t.Parallel()
	got := event.ResolveCurrent([]event.Session{{Title: "Untimed", SpeakerName: "Ada", SessionType: "PRESENTATION"}}, time.Now().UTC())
	if got.ClockMatch || got.Session != nil || len(got.Candidates) != 1 {
		t.Fatalf("got %+v", got)
	}
}
