package zipcheck_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

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
		"no MaxFileSize":     {MaxScanSize: 1, MaxFiles: 1, MaxRecursion: 17},
		"no MaxScanSize":     {MaxFileSize: 1, MaxFiles: 1, MaxRecursion: 17},
		"no MaxFiles":        {MaxFileSize: 1, MaxScanSize: 1, MaxRecursion: 17},
		"MaxFiles too large": {MaxFileSize: 1, MaxScanSize: 1, MaxFiles: 1 << 16, MaxRecursion: 17},
		"MaxRecursion 1":     {MaxFileSize: 1, MaxScanSize: 1, MaxFiles: 1, MaxRecursion: 1},
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
