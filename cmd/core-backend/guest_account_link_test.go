package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/commpref"
)

// The command takes at most one of -apply, -off, -undo and refuses
// anything else before it reads Keycloak or the database; without its
// settings it names what is missing.
func TestGuestAccountLinkCommandFlags(t *testing.T) {
	t.Parallel()
	read := func(context.Context) (map[string]commpref.VerifiedAddress, error) {
		t.Fatal("read Keycloak for a refused command")
		return nil, nil
	}
	for _, args := range [][]string{{"-apply", "-undo"}, {"-off", "-apply"}, {"extra"}, {"-unknown"}} {
		var out bytes.Buffer
		if code := guestAccountLinkCommand(context.Background(), args, &out, read, nil); code != 2 {
			t.Fatalf("%v: exit %d (%s)", args, code, out.String())
		}
	}
	var out bytes.Buffer
	if code := runGuestAccountLink(nil, func(string) string { return "" }, &out); code != 2 || !strings.Contains(out.String(), "DATABASE_URL") {
		t.Fatalf("without settings: exit %d %q", code, out.String())
	}
}
