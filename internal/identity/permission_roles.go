package identity

import (
	"fmt"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// PermissionRolesWarning turns the startup check of the contract's roles
// (authz.PermissionRoles, docs/authz-roles.md) into one log line, or "" when
// every role exists. Keycloak's reconciler creates them (e-skylab-keycloak,
// KEYCLOAK_RECONCILE_ONLY=core-roles); core only reads. Missing roles do
// not stop core: in the groups mode nothing reads them yet, in the both mode
// the Privileged Groups still decide, and in the roles mode the actions they
// stand for are refused to everyone until they exist and are mapped.
func PermissionRolesWarning(clientID string, mode authz.RoleMode, missing []string, err error) string {
	if err != nil {
		return fmt.Sprintf("authz permission roles could not be checked on Keycloak client %s (%s=%s): %v", clientID, authz.RoleModeEnv, mode, err)
	}
	if len(missing) == 0 {
		return ""
	}
	consequence := "switch to both or roles only after Keycloak creates and maps them"
	switch mode {
	case authz.RoleModeBoth:
		consequence = "the Privileged Groups still decide; do not switch to roles"
	case authz.RoleModeRoles:
		consequence = "nobody may do what they stand for; set " + authz.RoleModeEnv + "=both until Keycloak creates and maps them"
	}
	return fmt.Sprintf("authz permission roles missing on Keycloak client %s (%s=%s): %s; %s",
		clientID, authz.RoleModeEnv, mode, strings.Join(missing, ", "), consequence)
}
