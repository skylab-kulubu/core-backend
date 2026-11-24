package main

import (
	"testing"
	"time"
)

// An unset SKYMAIL_URL leaves core running with mail off; a set one is used
// as it is.
func TestNewSkyMailWithoutURLKeepsCoreRunning(t *testing.T) {
	sky := newSkyMail("https://auth.example.test", "e-skylab", "core", "secret", "  ", time.Second)
	if sky == nil || !sky.URLMissing || sky.BaseURL != "" {
		t.Fatalf("missing: %+v", sky)
	}
	set := newSkyMail("https://auth.example.test", "e-skylab", "core", "secret", "http://skymail:3000/", time.Second)
	if set.URLMissing || set.BaseURL != "http://skymail:3000" {
		t.Fatalf("set: %+v", set)
	}
}
