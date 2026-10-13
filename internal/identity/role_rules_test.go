package identity_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

func TestParseDirectUserRoles(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{"": false, "  ": false, "off": false, " on ": true, "on": true} {
		got, err := identity.ParseDirectUserRoles(raw)
		if err != nil || got != want {
			t.Errorf("%q: %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"true", "ON", "yes", "1", "deny"} {
		if _, err := identity.ParseDirectUserRoles(raw); err == nil || !strings.Contains(err.Error(), identity.DirectUserRolesEnv) {
			t.Errorf("%q: %v, want an error naming %s", raw, err, identity.DirectUserRolesEnv)
		}
	}
}
