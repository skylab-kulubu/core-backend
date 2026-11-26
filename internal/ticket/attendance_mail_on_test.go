package ticket

import "testing"

func TestAttendanceMailOnFollowsNotifyAttendance(t *testing.T) {
	t.Parallel()
	s := &PostgresStore{}
	if w, r := s.AttendanceMailOn(); w || r {
		t.Fatalf("before the worker: %v %v, want off", w, r)
	}
	s.NotifyAttendance(true, false)
	if w, r := s.AttendanceMailOn(); !w || r {
		t.Fatalf("welcome only: %v %v", w, r)
	}
	s.NotifyAttendance(false, true)
	if w, r := s.AttendanceMailOn(); w || !r {
		t.Fatalf("recorded only: %v %v", w, r)
	}
}
