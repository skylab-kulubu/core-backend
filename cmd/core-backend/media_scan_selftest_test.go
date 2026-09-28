package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/clamd/clamdtest"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// The deploy check: inside core's container, the EICAR test file sent to
// clamd must be reported. It exits 0 only then, and says what clamd
// answered.
func TestMediaScanSelfTestPassesOnlyWhenClamdReportsEICAR(t *testing.T) {
	fake := clamdtest.New(t)
	var out, errOut bytes.Buffer
	if code := runMediaScanSelfTest(nil, env(map[string]string{"MEDIA_CLAMAV_ADDR": fake.Addr()}), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{fake.Addr(), clamdtest.Version, clamdtest.Signature, "FOUND"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}

	// -addr overrides the environment: the wizard tests a clamd before
	// core's env names it.
	out.Reset()
	if code := runMediaScanSelfTest([]string{"-addr", fake.Addr()}, env(nil), &out, &errOut); code != 0 {
		t.Fatalf("-addr: exit %d: %s", code, errOut.String())
	}

	blind := clamdtest.New(t)
	blind.Report(clamd.EICAR(), "")
	errOut.Reset()
	if code := runMediaScanSelfTest([]string{"-addr", blind.Addr()}, env(nil), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "clean") {
		t.Fatalf("a clamd that misses EICAR: exit %d: %s", code, errOut.String())
	}

	down := clamdtest.New(t)
	down.Stop()
	errOut.Reset()
	if code := runMediaScanSelfTest([]string{"-addr", down.Addr()}, env(nil), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "unreachable") {
		t.Fatalf("a clamd that is down: exit %d: %s", code, errOut.String())
	}
}

func TestMediaScanSelfTestNeedsAnAddress(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runMediaScanSelfTest(nil, env(nil), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "MEDIA_CLAMAV_ADDR") {
		t.Fatalf("no address: exit %d: %s", code, errOut.String())
	}
	errOut.Reset()
	if code := runMediaScanSelfTest([]string{"-addr", "clamav"}, env(nil), &out, &errOut); code != 2 {
		t.Fatalf("a malformed address: exit %d: %s", code, errOut.String())
	}
	if code := runMediaScanSelfTest([]string{"-nope"}, env(nil), &out, &errOut); code != 2 {
		t.Fatalf("an unknown flag: exit %d", code)
	}
}
