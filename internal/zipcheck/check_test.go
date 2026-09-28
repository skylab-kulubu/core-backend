package zipcheck_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// A refusal names members by their place, never by their names, so it can
// be logged.
func TestCheckRefusalNamesNoMember(t *testing.T) {
	t.Parallel()
	r := refused(t, build(t, stored("a.txt", nil), deflated("Ada Lovelace notlar.bin", zeros(2<<20))), limits, zipcheck.ErrTooLarge)
	if r.Reason != "member 2 inflates to 2097152 bytes, more than MaxFileSize (1048576)" {
		t.Fatalf("reason %q", r.Reason)
	}
	nested := build(t, deflated("iç.zip", build(t, stored("a", nil), deflated("Ada.bin", zeros(2<<20)))))
	if r := refused(t, nested, limits, zipcheck.ErrTooLarge); r.Reason != "member 2 of member 1 inflates to 2097152 bytes, more than MaxFileSize (1048576)" {
		t.Fatalf("reason %q", r.Reason)
	}
}

// Limits the check cannot hold a ZIP within, and storage that fails or
// serves less than the size, are errors that say nothing about the ZIP:
// never a refusal.
func TestCheckFailuresAreNoRefusal(t *testing.T) {
	t.Parallel()
	data := build(t, deflated("a.txt", bytes.Repeat([]byte("a"), 3000)))
	for name, l := range map[string]zipcheck.Limits{
		"no MaxFileSize":     {MaxScanSize: 1, MaxFiles: 1, MaxRecursion: 17, MaxBuffer: 1},
		"MaxFiles too large": {MaxFileSize: 1, MaxScanSize: 1, MaxFiles: 1 << 16, MaxRecursion: 17, MaxBuffer: 1},
		"no MaxBuffer":       {MaxFileSize: 1, MaxScanSize: 1, MaxFiles: 1, MaxRecursion: 17},
	} {
		if err, _ := check(t, data, l); err == nil || errors.Is(err, zipcheck.ErrTooLarge) || errors.Is(err, zipcheck.ErrInvalid) {
			t.Errorf("%s: err = %v, want an error that is no refusal", name, err)
		}
	}
	outage := errors.New("r2: 503 Service Unavailable")
	if err := zipcheck.Check(context.Background(), &source{data: data, fail: outage}, int64(len(data)), limits); !errors.Is(err, outage) {
		t.Errorf("storage down: err = %v", err)
	}
	// The object holds less than its size: its first bytes are gone.
	short := &shortSource{source{data: data}, 40}
	if err := zipcheck.Check(context.Background(), short, int64(len(data)), limits); err == nil ||
		errors.Is(err, zipcheck.ErrTooLarge) || errors.Is(err, zipcheck.ErrInvalid) {
		t.Errorf("short object: err = %v", err)
	}
}

// Limits are whole or not at all: one left out fails when they are set up
// (at startup), never in each check.
func TestLimitsMustBeWhole(t *testing.T) {
	t.Parallel()
	if err := limits.Validate(); err != nil {
		t.Fatalf("whole limits: %v", err)
	}
	for name, change := range map[string]func(*zipcheck.Limits){
		"no MaxFileSize":     func(l *zipcheck.Limits) { l.MaxFileSize = 0 },
		"no MaxScanSize":     func(l *zipcheck.Limits) { l.MaxScanSize = 0 },
		"no MaxFiles":        func(l *zipcheck.Limits) { l.MaxFiles = 0 },
		"MaxFiles too large": func(l *zipcheck.Limits) { l.MaxFiles = 1 << 16 },
		"MaxRecursion 1":     func(l *zipcheck.Limits) { l.MaxRecursion = 1 },
		"no MaxBuffer":       func(l *zipcheck.Limits) { l.MaxBuffer = 0 },
	} {
		l := limits
		change(&l)
		if err := l.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

// A check is timed by what the ZIP inflates to, not by its size: two
// minutes, two seconds per MiB of the file, and a second per MiB it
// inflates to (the declared total, never past MaxScanSize). A check past its
// time stops with ErrTookTooLong, which is no refusal.
func TestCheckIsTimedByWhatTheZIPInflatesTo(t *testing.T) {
	t.Parallel()
	if got, want := zipcheck.Timeout(10<<20, 0), 2*time.Minute+20*time.Second; got != want {
		t.Fatalf("Timeout(10 MiB, 0) = %v, want %v", got, want)
	}
	if got, want := zipcheck.Timeout(1<<20, 1<<30), 2*time.Minute+2*time.Second+1024*time.Second; got != want {
		t.Fatalf("Timeout(1 MiB, 1 GiB) = %v, want %v", got, want)
	}
	data := build(t, deflated("zeros.bin", zeros(900<<10)), deflated("b.txt", []byte("b")))
	start := time.Now()
	for name, c := range map[string]struct {
		elapsed time.Duration
		want    error
	}{
		// 2 minutes, 0 MiB of file, 0 MiB inflated (900 KiB rounds down).
		"within its time": {2*time.Minute - time.Second, nil},
		"past its time":   {2*time.Minute + time.Second, zipcheck.ErrTookTooLong},
	} {
		now := func() time.Time { return start.Add(c.elapsed) }
		err := zipcheck.CheckWithClock(context.Background(), &source{data: data}, int64(len(data)), limits, start, now)
		if !errors.Is(err, c.want) || errors.Is(err, zipcheck.ErrTooLarge) || errors.Is(err, zipcheck.ErrInvalid) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}
