package authz

import (
	"slices"
	"strings"
)

// Role classes (ADR 0065 decision 10, addendum of 2026-10-12; Yusuf:
// "sektör nasılsa öyle", Microsoft Entra's model). A Keycloak client role
// is one of two classes, named by its (client, role) pair, never by its
// name alone:
//
//   - an admin role (Entra's directory role): a Group that maps one, or
//     whose ancestor maps one, is a Protected group, managed by Privileged
//     only (Entra's role-assignable group);
//   - an app role (Entra's app role, bound to a Team or to the person): a
//     Leader may hand it out by changing the Group's membership.
//
// A role on neither list is unclassified and counts as an admin role
// (fail-closed): a new role is protected until someone classifies it, and
// core logs and counts it (TeamDelegation). Realm roles are always
// protected; they are not classified here. Changing either list is Yusuf's
// decision (docs/team-delegation.md lists both).

// RoleClass is the class of a client role.
type RoleClass string

const (
	// RoleClassApp is an app role: delegable.
	RoleClassApp RoleClass = "app"
	// RoleClassAdmin is a known admin role: protected.
	RoleClassAdmin RoleClass = "admin"
	// RoleClassUnclassified is a role on neither list: protected as an
	// admin role, and logged.
	RoleClassUnclassified RoleClass = "unclassified"
)

// RoleRef names client roles: Role of Client. An empty Client is every
// client; AnyRole is every role of Client.
type RoleRef struct {
	Client  string
	Role    string
	AnyRole bool
}

func (r RoleRef) matches(client, role string) bool {
	return (r.Client == "" || r.Client == client) && (r.AnyRole || r.Role == role)
}

// SiteClients are the Site clients: each is one site's tenant in the CMS
// (inscribed, ADR 0056). A new Site client's roles stay unclassified, so
// protected, until it is added here.
func SiteClients() []string {
	return []string{"frontend-main", "frontend-arge", "frontend-artlab", "frontend-yildizjam", "frontend-skydays"}
}

// AppRoles are the app roles: what each grants is bound to the holder
// themselves or to the Teams they are in, so a Leader may hand them out.
//   - core's team-bound certificate roles: only the Events of the holder's
//     Teams;
//   - core's own short link roles: only the holder's own links;
//   - Forms' seat (skyforms:access): only the forms shared with the holder;
//     form:manage, response:manage and feedback:manage only open Forms'
//     screens (forms-backend decides every form by its collaborators or
//     skyforms:*, which is an admin role);
//   - the broker's read-token (inside USER): the holder's own identity
//     provider token;
//   - SkyMail's access and template reading: opening SkyMail and offering a
//     mail for approval, which an approver still sends;
//   - every Site client's content:read: reading.
//
// news:read, planned for /UYELER (duyuru-haber), joins when it exists.
func AppRoles() []RoleRef {
	out := []RoleRef{
		{Client: "core", Role: "certificate:issue"},
		{Client: "core", Role: "certificate:revoke"},
		{Client: "core", Role: "certificate:template:manage"},
		{Client: "core", Role: "certificate:binding:manage"},
		{Client: "core", Role: "url:create"},
		{Client: "core", Role: "url:get"},
		{Client: "core", Role: "url:update"},
		{Client: "core", Role: "url:delete"},
		{Client: "core", Role: RoleURLAccess},
		{Client: "forms", Role: "skyforms:access"},
		{Client: "forms", Role: "skyforms:form:manage"},
		{Client: "forms", Role: "skyforms:response:manage"},
		{Client: "forms", Role: "skyforms:feedback:manage"},
		{Client: "broker", Role: "read-token"},
		{Client: "skymail", Role: "skymail:access"},
		{Client: "skymail", Role: "skymail:templates:read"},
	}
	for _, site := range SiteClients() {
		out = append(out, RoleRef{Client: site, Role: "content:read"})
	}
	return out
}

// RealmRoleUser is the realm role every member holds through /UYELER.
const RealmRoleUser = "USER"

// RoleCMSAccess is a Site client's editor role: content:read and
// content:write together, site-wide write (ADR 0056).
const RoleCMSAccess = "cms:access"

// AdminRoles are the known admin roles. Anything that is no app role is
// protected anyway; this list says what is known to be one, so the log
// tells a known admin role from one nobody has classified. A pair on both
// lists is an admin role. cms:access and content:write are named on every
// client: a Site editor's role is protected by its own name, whatever
// Keycloak answers about its composites.
func AdminRoles() []RoleRef {
	var out []RoleRef
	for _, role := range PermissionRoles() {
		if role != RoleURLAccess {
			out = append(out, RoleRef{Client: "core", Role: role})
		}
	}
	for _, role := range []string{RoleAuditRead, "users:read",
		// Service accounts' roles: never mapped to a Group.
		"media:attach", "ticket:forms", "url:forms", "consent:record", "consent:audience:read", "audience:read"} {
		out = append(out, RoleRef{Client: "core", Role: role})
	}
	for _, role := range []string{"client:admin", RoleContentWrite, RoleCMSAccess, "schema:sync"} {
		out = append(out, RoleRef{Role: role})
	}
	out = append(out, RoleRef{Client: "realm-management", AnyRole: true})
	for _, role := range []string{"skyforms:*", "skyforms:feedback:*", "skyforms:account:erase"} {
		out = append(out, RoleRef{Client: "forms", Role: role})
	}
	for _, role := range []string{"skymail:templates:write", "skymail:lists:read", "skymail:lists:write",
		"skymail:mails:read", "skymail:mails:write", "skymail:mails:send", "skymail:mails:approve",
		"skymail:system-mails:read", "skymail:announcements:send", "skymail:account:erase"} {
		out = append(out, RoleRef{Client: "skymail", Role: role})
	}
	out = append(out,
		RoleRef{Client: "place", Role: "place:admin"},
		RoleRef{Client: "place", Role: "place:moderator"},
		RoleRef{Client: "skycms", Role: "cms:account:erase"},
	)
	return out
}

var (
	appRoles   = AppRoles()
	adminRoles = AdminRoles()
)

// ClassifyRole is the class of role on clientID (a Keycloak client id).
func ClassifyRole(clientID, role string) RoleClass {
	for _, r := range adminRoles {
		if r.matches(clientID, role) {
			return RoleClassAdmin
		}
	}
	for _, r := range appRoles {
		if r.matches(clientID, role) {
			return RoleClassApp
		}
	}
	return RoleClassUnclassified
}

// DelegableRole reports whether a Group may map role of clientID and stay
// delegated: only an app role. Every other client role, a role nobody has
// classified included, makes the Group and every Group under it Protected.
// A composite app role is delegable only when every role it holds is too
// (the protection read walks it).
func DelegableRole(clientID, role string) bool {
	return ClassifyRole(clientID, role) == RoleClassApp
}

// Realm roles are classed by name and by where they sit, since core reads
// them as the names a Group holds in effect (it holds no view-realm, so a
// realm role's composites stay closed to it). Production's realm roles
// (measurement of 2026-10-12 22:42) mirror membership: USER on /UYELER
// (every member; its composite is broker/read-token), a Team's own name on
// that Team (WEBLAB on /UYELER/ARGE/WEBLAB) and <TEAM>_LEADER on that
// Team's Leader groups. Where they sit they grant what membership there
// already means, so they are app roles there; anywhere else they protect.

// adminRealmRoles are the known admin realm roles (the Java era's
// Privileged roles); every YK_ role is one too.
var adminRealmRoles = []string{"ADMIN", "YK", "DK", "Skymail-Admin", "admin"}

// reservedSegments are path names no realm role mirrors.
var reservedSegments = []string{"UYELER", "ADMIN", "YK", "DK", "USER"}

// ClassifyRealmRole is the class of the realm role role held in effect by
// the Group at groupPath:
//   - a known admin realm role (ADMIN, YK, DK, YK_…, Skymail-Admin): admin;
//   - USER: app (the member role every member holds through /UYELER);
//   - the name of a Group on groupPath under /UYELER (a Team or an area,
//     not a reserved or Leader name): app, the role mirrors membership of
//     that Group, which the holder already has;
//   - <NAME>_LEADER where groupPath runs through NAME's Leader group
//     (NAME/LIDERLER, NAME/KOORDINATORLER): app, the Team's Leader role on
//     its own Leader group;
//   - anything else: unclassified, protected as an admin role.
func (p Policy) ClassifyRealmRole(role, groupPath string) RoleClass {
	if slices.Contains(adminRealmRoles, role) || strings.HasPrefix(role, "YK_") {
		return RoleClassAdmin
	}
	if role == RealmRoleUser {
		return RoleClassApp
	}
	rest, ok := strings.CutPrefix(groupPath, "/UYELER/")
	if !ok || rest == "" {
		return RoleClassUnclassified
	}
	segments := strings.Split(rest, "/")
	for i, segment := range segments {
		if segment == "" || slices.Contains(reservedSegments, segment) || slices.Contains(p.LeaderSubgroups, segment) {
			continue
		}
		if role == segment {
			return RoleClassApp
		}
		if role == segment+"_LEADER" && i+1 < len(segments) && slices.Contains(p.LeaderSubgroups, segments[i+1]) {
			return RoleClassApp
		}
	}
	return RoleClassUnclassified
}
