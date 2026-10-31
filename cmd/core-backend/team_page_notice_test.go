package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// Every run is refused with exit 2, dry or -apply, whatever the environment
// holds; the environment is not even read, and nothing is printed on out.
func TestTeamPageNoticeAlwaysRefuses(t *testing.T) {
	full := map[string]string{
		identity.TeamPageGraceUntilEnv: "2026-11-10T21:00:00Z",
		"DATABASE_URL":                 "postgres://x",
		"KEYCLOAK_URL":                 "https://kc.example.test",
	}
	for _, args := range [][]string{nil, {"-apply"}, {"--apply"}, {"-send"}} {
		read := 0
		getenv := func(name string) string { read++; return full[name] }
		var out, errOut bytes.Buffer
		if code := runTeamPageNotice(args, getenv, &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
		if !strings.Contains(errOut.String(), "bu bildirim artık gönderilmez") {
			t.Errorf("%v: %q", args, errOut.String())
		}
		if out.Len() != 0 || read != 0 {
			t.Errorf("%v: printed %q, read %d variables", args, out.String(), read)
		}
	}
}
