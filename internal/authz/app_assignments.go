package authz

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/skylab-kulubu/core-backend/config"
)

// App assignments (ekip-yetkileri ticket 15; Microsoft Entra's Enterprise
// apps → an app → "Users and groups"): ADMIN and YK give an app role of an
// app to a Team, a Sub-team, a Leader group or a person from the admin
// panel, and take it off again; DK reads. Only app roles (AppRoles) are
// given this way. An admin role stays in code (Keycloak's reconciler), on a
// Privileged root or a role group under /ROLLER: Entra's role-assignable
// groups. docs/app-assignments.md.

// AppAssignmentsEnv switches the app assignment routes: off (the default)
// or on.
const AppAssignmentsEnv = "APP_ASSIGNMENTS"

// ParseAppAssignments reads AppAssignmentsEnv: empty or "off" is off, "on"
// is on, anything else is an error that names the variable.
func ParseAppAssignments(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s: %q is not %q or %q", AppAssignmentsEnv, raw, "off", "on")
	}
}

// App is one application of the catalogue (config/app-assignments.json):
// its Keycloak client, the name the panel shows, whether only the people it
// is assigned to may use it, and the app roles the panel assigns.
type App struct {
	Client string `json:"client"`
	Name   string `json:"name"`
	// AssignmentRequired is Entra's "Assignment required?": the app lets in
	// only the people it is assigned to, directly or through a Group. The
	// app enforces it by its own seat role; core and the panel only show it.
	AssignmentRequired bool `json:"assignmentRequired"`
	// Roles are the app roles the panel assigns, each an app role of
	// Client (ClassifyRole).
	Roles []string `json:"roles"`
	// SuggestedRoles are the roles the panel's add drawer selects at
	// first (SkyMail's "send for approval" pair); a part of Roles.
	SuggestedRoles []string `json:"suggestedRoles"`
}

type appCatalogue struct {
	Apps []App `json:"apps"`
}

// LoadApps reads the app catalogue carried in the binary
// (config/app-assignments.json) and checks it: every client once, a name,
// at least one role, every role an app role of its client and named once,
// the suggested roles among them. A catalogue that breaks a rule stops
// startup.
func LoadApps() ([]App, error) {
	return ParseApps(config.AppAssignments)
}

// ParseApps is LoadApps of raw.
func ParseApps(raw []byte) ([]App, error) {
	var c appCatalogue
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("app catalogue: %w", err)
	}
	if len(c.Apps) == 0 {
		return nil, fmt.Errorf("app catalogue: no app")
	}
	seen := map[string]bool{}
	for i, app := range c.Apps {
		switch {
		case strings.TrimSpace(app.Client) == "":
			return nil, fmt.Errorf("app catalogue: app %d has no client", i)
		case seen[app.Client]:
			return nil, fmt.Errorf("app catalogue: client %q is named twice", app.Client)
		case strings.TrimSpace(app.Name) == "":
			return nil, fmt.Errorf("app catalogue: %s has no name", app.Client)
		case len(app.Roles) == 0:
			return nil, fmt.Errorf("app catalogue: %s has no role", app.Client)
		}
		seen[app.Client] = true
		roles := map[string]bool{}
		for _, role := range app.Roles {
			if roles[role] {
				return nil, fmt.Errorf("app catalogue: %s names %q twice", app.Client, role)
			}
			if ClassifyRole(app.Client, role) != RoleClassApp {
				return nil, fmt.Errorf("app catalogue: %s %q is no app role (internal/authz/role_classes.go)", app.Client, role)
			}
			roles[role] = true
		}
		for _, role := range app.SuggestedRoles {
			if !roles[role] {
				return nil, fmt.Errorf("app catalogue: %s suggests %q, which it does not assign", app.Client, role)
			}
		}
		if c.Apps[i].SuggestedRoles == nil {
			c.Apps[i].SuggestedRoles = []string{}
		}
	}
	return c.Apps, nil
}

// AppOf is the app of apps whose client is client.
func AppOf(apps []App, client string) (App, bool) {
	i := slices.IndexFunc(apps, func(a App) bool { return a.Client == client })
	if i < 0 {
		return App{}, false
	}
	return apps[i], true
}

// The reasons an app assignment is refused (RoleRuleError's reason, code
// role_placement), besides the placement reasons the target shares with
// RolePlacement (members_root, area, outside_role_places).
const (
	// PlacementAdminRole: the role is no app role of the app (an admin
	// role, or one nobody has classified): it stays in code.
	PlacementAdminRole = "admin_role"
	// PlacementCodeArea: a Privileged root or a Group under one, /ROLLER or
	// a Group under it: their mappings are in code (the reconciler).
	PlacementCodeArea = "code_area"
	// PlacementServiceAccount: a client's service account; its roles are
	// in code.
	PlacementServiceAccount = "service_account"
	// PlacementInactiveUser: a person whose account is disabled, erased or
	// being erased.
	PlacementInactiveUser = "inactive_user"
	// PlacementUseAppAssignments: with the app assignments on, the Group
	// role mapping write (PUT /v1/groups/{id}/client-roles) changes no app
	// role of a Group the panel assigns to (AppAssignmentTarget ""): that
	// is POST and DELETE /v1/apps/{clientId}/assignments, with its row.
	PlacementUseAppAssignments = "use_app_assignments"
)

// AppAssignmentTarget is "" when an app role may be assigned from the panel
// to the Group at path: a Team, a Sub-team, or a Leader group of either
// (TeamOf). Otherwise it is the reason it may not: PlacementCodeArea for a
// Privileged root of roots or a Group under one and for /ROLLER or a Group
// under it, PlacementMembersRoot for /UYELER (every member: a default
// role), PlacementArea for an area, PlacementOutside for anything else.
func AppAssignmentTarget(path string, roots, leaders []string) string {
	switch {
	case PrivilegedRoot(roots, path) != "", path == RolesRoot, strings.HasPrefix(path, RolesRoot+"/"):
		return PlacementCodeArea
	case path == MembersRoot:
		return PlacementMembersRoot
	}
	if _, _, ok := TeamOf(path, roots, leaders); ok {
		return ""
	}
	if strings.HasPrefix(path, MembersRoot+"/") && len(pathSegments(path)) == 2 && !slices.Contains(leaders, pathSegments(path)[1]) {
		return PlacementArea
	}
	return PlacementOutside
}

// allowAppAssignment decides TypeAppAssignment: with the routes on
// (Policy.AppAssignments), a person (never a service account or a
// product) who may read Groups reads the assignments, and one who may
// change Groups' role mappings changes them. In the groups mode that is
// every Privileged root's member for a read and ADMIN's and YK's for a
// change (DK reads only); in the roles mode groups:manage (global:read
// for a read).
func (a *authorizer) allowAppAssignment(p Principal, action Action) bool {
	if !a.policy.AppAssignments || p.ServiceAccount || p.Product != "" || p.ID == "" {
		return false
	}
	switch action {
	case Read:
		return a.privilegedRead(p, RoleGroupsManage)
	case Update:
		return a.privileged(p, RoleGroupsManage)
	}
	return false
}
