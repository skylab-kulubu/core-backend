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
	// CodeKeycloakAdminRole: the write would map a realm-management role, or
	// a composite that holds one (at any depth), to a Group or a person:
	// Keycloak administration is given in Keycloak's console only (K3).
	CodeKeycloakAdminRole = "keycloak_admin_role"
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
//
// RoleHoldsKeycloakAdmin answers, for a client role about to be mapped,
// the realm-management role it is or holds as a composite at any depth,
// "" for none. A composite that holds a realm role answers that role: core
// cannot see into realm roles (no view-realm), so it counts as one that
// may hold an administration role (fail-closed; realm roles leave in F3).
type KeycloakAdminGroups interface {
	KeycloakAdminRole(ctx context.Context, groupID string) (string, error)
	RoleHoldsKeycloakAdmin(ctx context.Context, role ClientRole) (string, error)
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

// noKeycloakAdminRole refuses mapping any of roles when it is a
// realm-management role or holds one (K3): neither to a Group nor to a
// person, a service account included (its administration roles come from
// Keycloak's reconciler only). Without AdminGroups only the client's own
// roles are seen.
func (s *service) noKeycloakAdminRole(ctx context.Context, roles []ClientRole) error {
	for _, r := range roles {
		held := ""
		if r.ClientID == KeycloakAdminClient {
			held = r.Role
		} else if s.adminGroups != nil {
			var err error
			if held, err = s.adminGroups.RoleHoldsKeycloakAdmin(ctx, r); err != nil {
				return err
			}
		}
		if held != "" {
			return &RoleRuleError{Status: http.StatusForbidden, Code: CodeKeycloakAdminRole,
				Detail: "The role " + r.ClientID + ":" + r.Role + " is, or holds, a Keycloak administration role (" + KeycloakAdminClient + "): it is given in Keycloak's console only."}
		}
	}
	return nil
}

// addedRoles are the roles of roles the Group g does not map yet. Mappings
// that cannot be read count as none: every role is then an add.
func (s *service) addedRoles(ctx context.Context, g Group, roles []ClientRole) []ClientRole {
	before, err := s.dir.GroupClientRoles(ctx, g.ID)
	if err != nil {
		before = nil
	}
	var out []ClientRole
	for _, r := range roles {
		if !slices.ContainsFunc(before, func(b ClientRole) bool { return b.ClientID == r.ClientID && b.Role == r.Role }) {
			out = append(out, r)
		}
	}
	return out
}

// roleGroupStaysLeaf refuses a Group created under a role group
// (/ROLLER/<X>): its roles would flow down to it (review of #245). Role
// groups are leaves (S4).
func roleGroupStaysLeaf(parentPath string) error {
	if !strings.HasPrefix(parentPath, authz.RolesRoot+"/") {
		return nil
	}
	return &RoleRuleError{Status: http.StatusConflict, Code: CodeRolePlacement, Reason: authz.PlacementRolesGroupNotLeaf,
		Detail: "A role group under /ROLLER stays a leaf: a Group under it would hold its roles."}
}

// renameKeepsPlacement refuses renaming a Group that maps client roles in
// an allowed place (a top Team's Leader group, a role group, a top Team
// with app roles) to a name that puts them where the placement rule allows
// none (a top LIDERLER renamed into a Team: review of #245). A Group mapped
// against the rule already, from before it, renames as before.
func (s *service) renameKeepsPlacement(ctx context.Context, g Group, name string, hasSubgroups bool) error {
	roles, err := s.dir.GroupClientRoles(ctx, g.ID)
	if err != nil {
		return err
	}
	if len(roles) == 0 {
		return nil
	}
	if s.placementOf(g.Path, hasSubgroups, roles) != "" {
		return nil
	}
	path := g.Path[:strings.LastIndex(g.Path, "/")+1] + name
	reason := s.placementOf(path, hasSubgroups, roles)
	if reason == "" {
		return nil
	}
	return &RoleRuleError{Status: http.StatusConflict, Code: CodeRolePlacement, Reason: reason,
		Detail: "The Group maps roles; under the new name they would sit where no role is mapped. " + authz.PlacementDetail(reason)}
}

// placementOf is the first reason the placement rule (authz.RolePlacementOf)
// refuses one of roles at path, "" when it allows them all.
func (s *service) placementOf(path string, hasSubgroups bool, roles []ClientRole) string {
	leaders := authz.DefaultPolicy().LeaderSubgroups
	for _, r := range roles {
		if reason := authz.RolePlacementOf(path, s.privilegedRoots, leaders, hasSubgroups, r.ClientID, r.Role); reason != "" {
			return reason
		}
	}
	return ""
}

// placementAllows refuses a role mapping write that adds a role to a Group
// the placement rule (authz.RolePlacementOf) does not allow it on. Keeping
// or taking off an existing mapping is always allowed, so a Group mapped
// against the rule today can be cleaned. Mappings that cannot be read count
// as none: then every role is an add, and only an allowed Group is written.
func (s *service) placementAllows(ctx context.Context, g Group, added []ClientRole) error {
	if len(added) == 0 {
		return nil
	}
	subgroups, err := s.dir.Subgroups(ctx, g.ID)
	if err != nil {
		return err
	}
	reason := s.placementOf(g.Path, len(subgroups) > 0, added)
	if reason == "" {
		return nil
	}
	return &RoleRuleError{Status: http.StatusConflict, Code: CodeRolePlacement, Reason: reason, Detail: authz.PlacementDetail(reason)}
}
