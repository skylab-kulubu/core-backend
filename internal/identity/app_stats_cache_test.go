package identity

import (
	"errors"
	"testing"
	"time"
)

// The list's Group counts are kept a minute; a forget drops them, and a
// failed read, or one a forget overtook, is not kept (ticket 15, T4).
func TestAppStatsCache(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	c := newAppStatsCache(func() time.Time { return now })
	reads := 0
	read := func() (int, int, error) { reads++; return 2, 7, nil }
	get := func() {
		t.Helper()
		if in, members, err := c.of("g", read); err != nil || in != 2 || members != 7 {
			t.Fatalf("%d %d %v", in, members, err)
		}
	}
	get()
	get()
	if reads != 1 {
		t.Fatalf("reads %d within the minute", reads)
	}
	now = now.Add(appStatsTTL - time.Second)
	get()
	now = now.Add(time.Second)
	get()
	if reads != 2 {
		t.Fatalf("reads %d after the minute", reads)
	}
	c.forget()
	get()
	if reads != 3 {
		t.Fatalf("reads %d after a forget", reads)
	}
	if _, _, err := c.of("bad", func() (int, int, error) { return 0, 0, errors.New("down") }); err == nil {
		t.Fatal("no error")
	}
	if _, ok := c.groups["bad"]; ok {
		t.Fatal("a failed read was kept")
	}
	if _, _, err := c.of("raced", func() (int, int, error) { c.forget(); return 1, 1, nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.groups["raced"]; ok {
		t.Fatal("a read a forget overtook was kept")
	}
}
