package teammail

import (
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

// Kind is which membership mail a change gets (Yusuf, 2026-10-12: every
// Group's membership is mailed); each kind has its own SkyMail template.
type Kind = mail.MembershipKind

const (
	// KindTeam: a team or its LIDERLER/KOORDINATORLER (TeamOf),
	// club.team-membership.
	KindTeam = mail.MembershipTeam
	// KindRole: a Privileged root (AUTHZ_PRIVILEGED_ROOTS: YK, DK, ADMIN)
	// or a Group under one, club.role-membership.
	KindRole = mail.MembershipRole
	// KindGroup: every other Group (the club, an area, an area's own
	// LIDERLER, technical Groups, Groups outside /UYELER),
	// club.group-membership.
	KindGroup = mail.MembershipGroup
)

// KindOf is the kind of a membership of the Group at path, by the rule of
// this core: a team (TeamOf) first, then a Privileged root or a Group under
// one, then any other Group. A path that is no Group path ("", or one that
// does not start with "/") gets no mail.
func KindOf(path string, privilegedRoots []string) (Kind, bool) {
	if !strings.HasPrefix(path, "/") {
		return "", false
	}
	if _, ok := TeamOf(path, privilegedRoots); ok {
		return KindTeam, true
	}
	if authz.PrivilegedRoot(privilegedRoots, path) != "" {
		return KindRole, true
	}
	return KindGroup, true
}

// privilegedRole is the role mail's Role variable: which Privileged root
// the Group at path is, or is under, by the root's last segment. `board`
// (YK, Yönetim Kurulu), `audit` (DK, Denetim Kurulu), `admin` (ADMIN), and
// `privileged` for a root the template has no words for.
func privilegedRole(path string, privilegedRoots []string) string {
	root := authz.PrivilegedRoot(privilegedRoots, path)
	if root == "" {
		return "privileged"
	}
	switch authz.RootLabel(root) {
	case "YK":
		return "board"
	case "DK":
		return "audit"
	case "ADMIN":
		return "admin"
	}
	return "privileged"
}

// groupName is how a mail names a Group (Yusuf, 2026-10-09): its code (the
// Group's name) and its Turkish display name (display_name_tr), "WEBLAB ·
// Web Geliştirme". Without a Turkish name, or one that is the code itself,
// the code alone.
func groupName(group identity.Group) string {
	code := strings.TrimSpace(group.Name)
	if code == "" {
		code = group.Path[strings.LastIndex(group.Path, "/")+1:]
	}
	if turkish := strings.TrimSpace(group.Attributes["display_name_tr"]); turkish != "" && turkish != code {
		return code + " · " + turkish
	}
	return code
}
