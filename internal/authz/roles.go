package authz

import (
	"fmt"
	"strings"
)

// The roles of core's Keycloak client that stand for the Privileged Groups
// (ADR-0059). The names are the contract with Keycloak (spec
// .scratch/admin-token-authz "Sözleşme: core'un kaynak rolleri", ticket 03):
// whoever renames one changes that table first. Tokens carry them in
// resource_access.core.roles. See docs/authz-roles.md.
const (
	// RoleEventManage: Events, EventDays and Sessions of every Owner team
	// (create, update, delete) and an Event's door staff (Assign).
	RoleEventManage = "event:manage"
	// RoleSeasonManage: every Season action but Read.
	RoleSeasonManage = "season:manage"
	// RoleTicketManage: every Event's Tickets (Read) and assigning them.
	RoleTicketManage = "ticket:manage"
	// RoleTicketValidate: door check-in at every Event, SkyPass included.
	RoleTicketValidate = "ticket:validate"
	// RoleCompetitorManage: every competitor action for every Owner team.
	RoleCompetitorManage = "competitor:manage"
	// RoleMediaManage: the whole Media library (List) and deleting any Media.
	RoleMediaManage = "media:manage"
	// RoleMediaPrivateRead: a person opening core's own private Media.
	RoleMediaPrivateRead = "media:private:read"
	// RoleCertificateManage: certificates and certificate templates of every
	// Owner team. The team-bound certificate roles stay as they are.
	RoleCertificateManage = "certificate:manage"
	// RoleUsersManage: every User action (users:read still reads).
	RoleUsersManage = "users:manage"
	// RoleGroupsManage: every Group action, group role mappings included.
	RoleGroupsManage = "groups:manage"
	// RoleGithubActivityRead: the club's GitHub activity, people only.
	RoleGithubActivityRead = "github:activity:read"
	// RoleURLModerator existed before: everyone's short links and form links.
	RoleURLModerator = "url:moderator"
	// RoleURLAccess existed before: one's own short links.
	RoleURLAccess = "url:access"
)

// permissionRoles is the contract table in order. preexisting roles were
// granted to people outside the Privileged Groups before the table, so
// holding one without such a Group is not a disagreement.
var permissionRoles = []struct {
	name        string
	preexisting bool
}{
	{RoleEventManage, false},
	{RoleSeasonManage, false},
	{RoleTicketManage, false},
	{RoleTicketValidate, false},
	{RoleCompetitorManage, false},
	{RoleMediaManage, false},
	{RoleMediaPrivateRead, false},
	{RoleCertificateManage, false},
	{RoleUsersManage, false},
	{RoleGroupsManage, false},
	{RoleGithubActivityRead, false},
	{RoleURLModerator, true},
	{RoleURLAccess, true},
}

// PermissionRoles are the roles of the contract, in its order: the roles a
// Privileged member holds once Keycloak maps them to ADMIN, YK and DK.
func PermissionRoles() []string {
	out := make([]string, len(permissionRoles))
	for i, r := range permissionRoles {
		out[i] = r.name
	}
	return out
}

func preexistingRole(name string) bool {
	for _, r := range permissionRoles {
		if r.name == name {
			return r.preexisting
		}
	}
	return false
}

// RoleMode is where the Privileged decisions come from while the roles roll
// out (AUTHZ_ROLE_MODE).
type RoleMode string

const (
	// RoleModeGroups decides from the Privileged Group paths alone, as core
	// always has. The default: releasing core changes nothing.
	RoleModeGroups RoleMode = "groups"
	// RoleModeBoth allows when the Group or the role allows, and counts and
	// logs every check where the two disagree.
	RoleModeBoth RoleMode = "both"
	// RoleModeRoles decides from the roles alone; a Privileged Group grants
	// nothing by itself.
	RoleModeRoles RoleMode = "roles"
)

// RoleModeEnv names the role mode.
const RoleModeEnv = "AUTHZ_ROLE_MODE"

// ParseRoleMode reads RoleModeEnv: empty is groups, and anything but the
// three spellings is an error, so a typo cannot quietly change who may do
// what.
func ParseRoleMode(raw string) (RoleMode, error) {
	switch mode := RoleMode(strings.TrimSpace(raw)); mode {
	case "":
		return RoleModeGroups, nil
	case RoleModeGroups, RoleModeBoth, RoleModeRoles:
		return mode, nil
	default:
		return "", fmt.Errorf("%s: %q is not one of %q, %q, %q", RoleModeEnv, raw, RoleModeGroups, RoleModeBoth, RoleModeRoles)
	}
}
