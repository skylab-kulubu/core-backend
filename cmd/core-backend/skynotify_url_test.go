package main

import (
	"testing"
	"time"
)

// An unset SKYNOTIFY_URL leaves core running with mail off; a set one is used
// as it is.
func TestNewSkyNotifyWithoutURLKeepsCoreRunning(t *testing.T) {
	sky := newSkyNotify("https://auth.example.test", "e-skylab", "core", "secret", "  ", time.Second)
	if sky == nil || !sky.URLMissing || sky.BaseURL != "" {
		t.Fatalf("missing: %+v", sky)
	}
	set := newSkyNotify("https://auth.example.test", "e-skylab", "core", "secret", "http://skynotify:3000/", time.Second)
	if set.URLMissing || set.BaseURL != "http://skynotify:3000" {
		t.Fatalf("set: %+v", set)
	}
}
