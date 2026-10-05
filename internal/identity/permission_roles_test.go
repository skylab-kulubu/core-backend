package identity

import (
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

func TestPermissionRolesWarning(t *testing.T) {
	t.Parallel()
	if got := PermissionRolesWarning("core", authz.RoleModeRoles, nil, nil); got != "" {
		t.Fatalf("no missing role: %q", got)
	}
	got := PermissionRolesWarning("core", authz.RoleModeRoles, []string{"event:manage", "season:manage"}, nil)
	for _, want := range []string{"core", "AUTHZ_ROLE_MODE=roles", "event:manage, season:manage", "nobody may"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q lacks %q", got, want)
		}
	}
	if got := PermissionRolesWarning("core", authz.RoleModeGroups, []string{"event:manage"}, nil); !strings.Contains(got, "only after Keycloak") {
		t.Errorf("groups warning = %q", got)
	}
	if got := PermissionRolesWarning("core", authz.RoleModeBoth, nil, errors.New("boom")); !strings.Contains(got, "could not be checked") || !strings.Contains(got, "boom") {
		t.Errorf("error warning = %q", got)
	}
}
