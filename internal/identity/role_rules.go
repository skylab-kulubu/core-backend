package identity

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// Role rules of the role cleanup (sky_lab_genel .scratch/rol-temizligi,
// ticket 06; docs/team-delegation.md "Role rules"): access comes through
// Groups only (K1), a role is mapped only where the placement rule allows
// it (K4), and core changes no membership of a Group that holds a Keycloak
// administration role (K3, defence in depth).

// The problem codes of the role rules.
const (
	// CodeDirectRoleNotAllowed: POST /v1/users/{id}/client-roles for a
	// person (not a service account) while DIRECT_USER_ROLES is off.
	CodeDirectRoleNotAllowed = "direct_role_not_allowed"
	// CodeRolePlacement: PUT /v1/groups/{id}/client-roles would map a new
	// role to a Group the placement rule does not allow
	// (authz.RolePlacement); the problem's reason member says why.
	CodeRolePlacement = "role_placement"
	// CodeKeycloakAdminGroup: the Group, or one above it, holds a
	// realm-management role (directly or inside a composite): core adds
	// and removes nobody there.
	CodeKeycloakAdminGroup = "keycloak_admin_group"
)

// RoleRuleError is a request a role rule refused: its status, problem
// code, the sentence that says why and, for a placement, the reason
// (authz.Placement…).
type RoleRuleError struct {
	Status int
	Code   string
	Reason string
	Detail string
}

func (e *RoleRuleError) Error() string {
	return fmt.Sprintf("identity: role rule refused: %s %s (%d)", e.Code, e.Reason, e.Status)
}

// directRoleRefused is the refusal of a role given to a person directly.
func directRoleRefused() error {
	return &RoleRuleError{Status: http.StatusForbidden, Code: CodeDirectRoleNotAllowed,
		Detail: "A role is not given to a person directly: add them to the Group that holds it (a role group under /ROLLER, a top Team's Leader group or a Privileged root)."}
}

// DirectUserRolesEnv switches giving a client role to a person directly.
const DirectUserRolesEnv = "DIRECT_USER_ROLES"

// ParseDirectUserRoles reads DirectUserRolesEnv: empty or "off" is off
// (refused, the default), "on" is on; anything else is an error that names
// the variable.
func ParseDirectUserRoles(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s: %q is not %q or %q", DirectUserRolesEnv, raw, "off", "on")
	}
}

// DirectUserRolesFromEnv is ParseDirectUserRoles of the process's variable.
func DirectUserRolesFromEnv() (bool, error) {
	return ParseDirectUserRoles(os.Getenv(DirectUserRolesEnv))
}

// KeycloakAdminGroups reads whether a Group's own role mappings hold a
// Keycloak administration role: a role of the realm-management client,
// mapped or inside a composite (a client role's or a realm role's). The
// answer is the role's name, "" for none.
type KeycloakAdminGroups interface {
	KeycloakAdminRole(ctx context.Context, groupID string) (string, error)
}

// KeycloakAdminClient is the client whose roles administer the realm.
const KeycloakAdminClient = "realm-management"

// notKeycloakAdminGroup refuses a membership change of the Group groupRef
// when it, or a Group above it, holds a realm-management role. Rule K3
// keeps every such role off e-skylab's Groups, so this never refuses while
// K3 holds; it is defence in depth. A Group that cannot be read fails the
// change.
func (s *service) notKeycloakAdminGroup(ctx context.Context, groupRef string) error {
	if s.adminGroups == nil {
		return nil
	}
	g, err := s.dir.GetGroup(ctx, groupRef)
	if err != nil {
		return err
	}
	ids := []string{g.ID}
	segments := strings.Split(strings.TrimPrefix(g.Path, "/"), "/")
	for i := len(segments) - 1; i >= 1; i-- {
		above, err := s.dir.GetGroup(ctx, "/"+strings.Join(segments[:i], "/"))
		if err != nil {
			return err
		}
		ids = append(ids, above.ID)
	}
	for _, id := range ids {
		role, err := s.adminGroups.KeycloakAdminRole(ctx, id)
		if err != nil {
			return err
		}
		if role != "" {
			return &RoleRuleError{Status: http.StatusForbidden, Code: CodeKeycloakAdminGroup,
				Detail: "This Group, or one above it, holds a Keycloak administration role (" + KeycloakAdminClient + "): its membership is changed in Keycloak's console only, by the console's writers."}
		}
	}
	return nil
}

// placementAllows refuses a role mapping write that adds a role to a Group
// the placement rule (authz.RolePlacement) does not allow. Keeping or
// taking off an existing mapping is always allowed, so a Group mapped
// against the rule today can be cleaned. Mappings that cannot be read count
// as none: then every role is an add, and only an allowed Group is written.
func (s *service) placementAllows(ctx context.Context, g Group, roles []ClientRole) error {
	before, err := s.dir.GroupClientRoles(ctx, g.ID)
	if err != nil {
		before = nil
	}
	adds := slices.ContainsFunc(roles, func(r ClientRole) bool {
		return !slices.ContainsFunc(before, func(b ClientRole) bool { return b.ClientID == r.ClientID && b.Role == r.Role })
	})
	if !adds {
		return nil
	}
	subgroups, err := s.dir.Subgroups(ctx, g.ID)
	if err != nil {
		return err
	}
	reason := authz.RolePlacement(g.Path, s.privilegedRoots, authz.DefaultPolicy().LeaderSubgroups, len(subgroups) > 0)
	if reason == "" {
		return nil
	}
	return &RoleRuleError{Status: http.StatusConflict, Code: CodeRolePlacement, Reason: reason, Detail: authz.PlacementDetail(reason)}
}
