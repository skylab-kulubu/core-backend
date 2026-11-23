package organizer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeTeams struct {
	mu    sync.Mutex
	calls int
	names map[string]string
	err   error
	done  chan struct{}
}

func (f *fakeTeams) read(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.done != nil {
		defer func() { f.done <- struct{}{} }()
	}
	return f.names, f.err
}

func (f *fakeTeams) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Not parallel: UseNames is the process's one setting.
func TestNameWithTurkishNames(t *testing.T) {
	src := &fakeTeams{names: map[string]string{"WEBLAB": " Web Geliştirme ", "AGC": "AGC", "YK": "Yönetim Kurulu"}}
	UseNames(NewNames(src.read, time.Hour, nil))
	t.Cleanup(func() { UseNames(nil) })
	for team, want := range map[string]string{
		"WEBLAB":   "WEBLAB · Web Geliştirme",
		" WEBLAB ": "WEBLAB · Web Geliştirme",
		"AGC":      "AGC", // its name is its code
		"GECEKODU": "GECEKODU",
		"YK":       ClubName,
		"":         ClubName,
	} {
		if got := Name(team); got != want {
			t.Errorf("Name(%q) = %q, want %q", team, got, want)
		}
	}
	if src.count() != 1 {
		t.Fatalf("read %d times, want once", src.count())
	}
}

// The names are read once and kept for the TTL; after it the old ones answer
// while they are read again in the background.
func TestNamesCacheAndRefresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	src := &fakeTeams{names: map[string]string{"WEBLAB": "Web Geliştirme"}}
	n := NewNames(src.read, 15*time.Minute, nil)
	var clock sync.Mutex
	n.now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	for range 3 {
		if got := n.Turkish("WEBLAB"); got != "Web Geliştirme" {
			t.Fatalf("Turkish %q", got)
		}
	}
	if src.count() != 1 {
		t.Fatalf("read %d times within the TTL", src.count())
	}
	src.mu.Lock()
	src.names = map[string]string{"WEBLAB": "Web Laboratuvarı"}
	src.done = make(chan struct{}, 1)
	src.mu.Unlock()
	clock.Lock()
	now = now.Add(16 * time.Minute)
	clock.Unlock()
	if got := n.Turkish("WEBLAB"); got != "Web Geliştirme" {
		t.Fatalf("stale read answered %q, want the old name while refreshing", got)
	}
	<-src.done
	deadline := time.Now().Add(2 * time.Second)
	for n.Turkish("WEBLAB") != "Web Laboratuvarı" {
		if time.Now().After(deadline) {
			t.Fatal("the background read never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if src.count() != 2 {
		t.Fatalf("read %d times, want 2", src.count())
	}
}

// A read that fails answers no name (the code), is told, and is not tried
// again on every call: only after the retry.
func TestNamesFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	src := &fakeTeams{err: errors.New("keycloak down")}
	var told int
	n := NewNames(src.read, time.Hour, func(error) { told++ })
	n.now = func() time.Time { return now }
	for range 5 {
		if got := n.Turkish("WEBLAB"); got != "" {
			t.Fatalf("Turkish %q, want none", got)
		}
	}
	if src.count() != 1 || told != 1 {
		t.Fatalf("read %d times, told %d", src.count(), told)
	}
}
