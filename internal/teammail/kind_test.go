package teammail

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// Every Group's membership is mailed (Yusuf, 2026-10-12); the kind picks the
// template: a team's, a Privileged Group's or any other Group's.
func TestKindOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path string
		kind Kind
		ok   bool
	}{
		{"/UYELER/ARGE/WEBLAB", KindTeam, true},
		{"/UYELER/ARGE/WEBLAB/LIDERLER", KindTeam, true},
		{"/UYELER/ORGANIZASYON/ARTLAB/KOORDINATORLER", KindTeam, true},
		{"/UYELER/ARGE/ALGOLAB/AGC", KindTeam, true},
		// A Group named like a root elsewhere is a team like any other.
		{"/UYELER/ARGE/YK", KindTeam, true},
		{"/UYELER/ARGE/WEBLAB/ADMIN", KindTeam, true},
		// The Privileged roots and everything under them.
		{"/ADMIN", KindRole, true},
		{"/UYELER/ADMIN", KindRole, true},
		{"/UYELER/YK", KindRole, true},
		{"/UYELER/YK/BASKAN", KindRole, true},
		{"/UYELER/DK/LIDERLER", KindRole, true},
		{"/YK", KindRole, true},
		// Everything else: the club, an area, an area's leaders, technical
		// Groups, Groups outside /UYELER.
		{"/UYELER", KindGroup, true},
		{"/UYELER/ARGE", KindGroup, true},
		{"/UYELER/ARGE/LIDERLER", KindGroup, true},
		{"/UYELER/ESKI-EDITORLER", KindGroup, true},
		{"/UYELER/ARGE/WEBLAB/LIDERLER/X", KindGroup, true},
		{"/SERVIS/ARGE/WEBLAB", KindGroup, true},
		{"/YKX", KindGroup, true},
		// No Group path at all.
		{"", "", false},
		{"UYELER/ARGE/WEBLAB", "", false},
	} {
		kind, ok := KindOf(tc.path, authz.DefaultPrivilegedRoots())
		if kind != tc.kind || ok != tc.ok {
			t.Errorf("KindOf(%q) = %q %v; want %q %v", tc.path, kind, ok, tc.kind, tc.ok)
		}
	}
}

// A narrowed AUTHZ_PRIVILEGED_ROOTS decides, not the default six: a Group at
// a default root left out of it (/UYELER/ADMIN) is then no Privileged Group
// and gets the group mail, as authz no longer counts it Privileged either.
func TestKindOfFollowsTheConfiguredRoots(t *testing.T) {
	t.Parallel()
	roots := []string{"/ADMIN", "/UYELER/YK", "/UYELER/DK"}
	if kind, _ := KindOf("/UYELER/ADMIN", roots); kind != KindGroup {
		t.Fatalf("/UYELER/ADMIN = %q", kind)
	}
	if kind, _ := KindOf("/UYELER/DK", roots); kind != KindRole {
		t.Fatalf("/UYELER/DK = %q", kind)
	}
}

// The role mail words its text by the root: Yönetim Kurulu, Denetim Kurulu,
// the administrators, or a root it has no words for.
func TestPrivilegedRoleOf(t *testing.T) {
	t.Parallel()
	roots := authz.DefaultPrivilegedRoots()
	for path, want := range map[string]string{
		"/UYELER/YK":        "board",
		"/UYELER/YK/BASKAN": "board",
		"/YK":               "board",
		"/UYELER/DK":        "audit",
		"/DK/UYE":           "audit",
		"/ADMIN":            "admin",
		"/UYELER/ADMIN":     "admin",
	} {
		if got := privilegedRole(path, roots); got != want {
			t.Errorf("privilegedRole(%q) = %q, want %q", path, got, want)
		}
	}
	if got := privilegedRole("/OPS/ROOT", []string{"/OPS/ROOT"}); got != "privileged" {
		t.Fatalf("custom root = %q", got)
	}
}

func TestGroupNameByCodeAndTurkishName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		group identity.Group
		want  string
	}{
		{identity.Group{Name: "YK", Path: "/UYELER/YK", Attributes: map[string]string{"display_name_tr": "Yönetim Kurulu"}}, "YK · Yönetim Kurulu"},
		{identity.Group{Name: "ARGE", Path: "/UYELER/ARGE", Attributes: map[string]string{"display_name_tr": "ARGE"}}, "ARGE"},
		{identity.Group{Path: "/UYELER/ESKI-EDITORLER"}, "ESKI-EDITORLER"},
	} {
		if got := groupName(tc.group); got != tc.want {
			t.Errorf("groupName(%+v) = %q, want %q", tc.group, got, tc.want)
		}
	}
}
