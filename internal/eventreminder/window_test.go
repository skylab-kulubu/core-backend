package eventreminder

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func istanbul(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(timeZone)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// The plan opens at 18:00 Istanbul time and covers the next Istanbul day
// until midnight; before 18:00, and after midnight (the Event's own day),
// nothing. A UTC clock reads the same: 15:00 UTC is 18:00 in Istanbul.
func TestPlanWindow(t *testing.T) {
	t.Parallel()
	loc := istanbul(t)
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, loc) }
	for _, tc := range []struct {
		name     string
		now      time.Time
		ok       bool
		from, to time.Time
	}{
		{"17:59", at(24, 17, 59), false, time.Time{}, time.Time{}},
		{"18:00", at(24, 18, 0), true, at(25, 0, 0), at(26, 0, 0)},
		{"18:00 read in UTC", time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC), true, at(25, 0, 0), at(26, 0, 0)},
		{"23:59", at(24, 23, 59), true, at(25, 0, 0), at(26, 0, 0)},
		{"midnight", at(25, 0, 0), false, time.Time{}, time.Time{}},
		{"month end", at(30, 18, 30), true, time.Date(2026, 12, 1, 0, 0, 0, 0, loc), time.Date(2026, 12, 2, 0, 0, 0, 0, loc)},
		{"year end", time.Date(2026, 12, 31, 20, 0, 0, 0, loc), true, time.Date(2027, 1, 1, 0, 0, 0, 0, loc), time.Date(2027, 1, 2, 0, 0, 0, 0, loc)},
	} {
		w, ok := PlanWindow(tc.now, loc)
		if ok != tc.ok {
			t.Fatalf("%s: ok = %v", tc.name, ok)
		}
		if !ok {
			continue
		}
		if !w.From.Equal(tc.from) || !w.To.Equal(tc.to) || !w.After.Equal(tc.now.Add(MissedPlanCutoff)) {
			t.Fatalf("%s: window %v – %v after %v", tc.name, w.From, w.To, w.After)
		}
	}
}

// Istanbul keeps UTC+3 all year: the window is 24 hours on the days other
// zones change their clocks.
func TestPlanWindowHasNoDaylightSaving(t *testing.T) {
	t.Parallel()
	loc := istanbul(t)
	for _, day := range []time.Time{
		time.Date(2026, 3, 28, 18, 0, 0, 0, loc),  // Europe's spring change is the next night
		time.Date(2026, 10, 24, 18, 0, 0, 0, loc), // and its autumn change
	} {
		w, ok := PlanWindow(day, loc)
		if !ok || w.To.Sub(w.From) != 24*time.Hour {
			t.Fatalf("%v: window %v – %v", day, w.From, w.To)
		}
		if _, offset := w.From.Zone(); offset != 3*3600 {
			t.Fatalf("%v: offset %d", day, offset)
		}
	}
}

func TestIdempotencyKey(t *testing.T) {
	t.Parallel()
	ev := uuid.MustParse("6f1c2b9e-4d3a-4b7e-9a51-0c8d2e7f4a19")
	tk := uuid.MustParse("0b0c0d0e-0000-4000-8000-000000000001")
	starts := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	want := "core:event.reminder:6f1c2b9e-4d3a-4b7e-9a51-0c8d2e7f4a19:1795600800:0b0c0d0e-0000-4000-8000-000000000001"
	if got := IdempotencyKey(ev, starts, tk); got != want {
		t.Fatalf("key %q", got)
	}
}
