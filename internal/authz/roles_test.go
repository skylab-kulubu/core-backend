package authz

import (
	"bytes"
	"log"
	"slices"
	"strings"
	"testing"
	"time"
)

// asRoleHolder is the same person as p with the Privileged Group paths
// swapped for every role of the contract: what the person's token carries
// once Keycloak maps the roles to ADMIN, YK and DK and the person leaves
// those Groups out of the picture.
func asRoleHolder(p Principal) Principal {
	out := p
	out.Groups = nil
	privileged := false
	for _, g := range p.Groups {
		if inPrivilegedGroup(DefaultPolicy(), g) != "" {
			privileged = true
			continue
		}
		out.Groups = append(out.Groups, g)
	}
	if privileged {
		out.Roles = append(slices.Clone(p.Roles), PermissionRoles()...)
	}
	return out
}

func inPrivilegedGroup(policy Policy, g string) string {
	for _, pg := range policy.PrivilegedGroups {
		if strings.HasSuffix(g, "/"+pg) || strings.Contains(g, "/"+pg+"/") {
			return pg
		}
	}
	return ""
}

func policyIn(mode RoleMode) Policy {
	policy := DefaultPolicy()
	policy.RoleMode = mode
	return policy
}

// The same people decide the same in every mode once their roles stand for
// their Privileged Groups; Leader and Owner team decisions do not move.
func TestAuthorizer_RolesDecideAsGroupsDid(t *testing.T) {
	t.Parallel()
	for _, mode := range []RoleMode{RoleModeGroups, RoleModeBoth, RoleModeRoles} {
		auth := NewAuthorizer(policyIn(mode))
		for _, tt := range allowCases() {
			p := tt.p
			if mode == RoleModeRoles {
				p = asRoleHolder(p)
			}
			if got := auth.Allow(p, tt.r, tt.a); got != tt.want {
				t.Errorf("%s: %s: Allow() = %v, want %v", mode, tt.name, got, tt.want)
			}
		}
	}
}

// In the both mode a role holder outside the Privileged Groups and a
// Privileged member without roles are both allowed.
func TestAuthorizer_BothModeAllowsGroupOrRole(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(policyIn(RoleModeBoth))
	for _, tt := range allowCases() {
		if got := auth.Allow(asRoleHolder(tt.p), tt.r, tt.a); got != tt.want {
			t.Errorf("roles only: %s: Allow() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// One check per role of the contract: what the role grants, which nothing
// else in the principal grants.
var roleChecks = []struct {
	role string
	r    Resource
	a    Action
}{
	{RoleEventManage, Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Delete},
	{RoleEventManage, Resource{Type: TypeEventDay, OwnerTeam: "WEBLAB"}, Update},
	{RoleEventManage, Resource{Type: TypeSession}, Create},
	{RoleEventManage, Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Assign},
	{RoleSeasonManage, Resource{Type: TypeSeason}, Create},
	{RoleTicketManage, Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"}, Read},
	{RoleTicketManage, Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"}, Assign},
	{RoleTicketValidate, Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"}, Validate},
	{RoleTicketValidate, Resource{Type: TypeTicket}, Validate},
	{RoleCompetitorManage, Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB"}, Update},
	{RoleMediaManage, Resource{Type: TypeMedia}, List},
	{RoleMediaManage, Resource{Type: TypeMedia}, Delete},
	{RoleMediaPrivateRead, Resource{Type: TypeMediaReadLink}, Read},
	{RoleCertificateManage, Resource{Type: TypeCertificate, OwnerTeam: "WEBLAB"}, Issue},
	{RoleCertificateManage, Resource{Type: TypeCertificate}, Revoke},
	{RoleCertificateManage, Resource{Type: TypeCertificateTemplate}, Create},
	{RoleUsersManage, Resource{Type: TypeUser}, Update},
	{RoleUsersManage, Resource{Type: TypeUser}, Delete},
	{RoleGroupsManage, Resource{Type: TypeGroup}, Update},
	{RoleGithubActivityRead, Resource{Type: TypeGithubActivity}, Read},
	{RoleURLModerator, Resource{Type: TypeFormLink}, Update},
}

func TestAuthorizer_RolesModeReadsTheRoleNotTheGroup(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(policyIn(RoleModeRoles))
	groups := NewAuthorizer(policyIn(RoleModeGroups))
	for _, c := range roleChecks {
		yk := Principal{ID: "u1", Groups: []string{"/UYELER/YK"}}
		if auth.Allow(yk, c.r, c.a) {
			t.Errorf("%s %s: YK without the role allowed in the roles mode", c.r.Type, c.a)
		}
		holder := Principal{ID: "u1", Roles: []string{c.role}}
		if !auth.Allow(holder, c.r, c.a) {
			t.Errorf("%s %s: %s holder refused in the roles mode", c.r.Type, c.a, c.role)
		}
		if !preexistingRole(c.role) && groups.Allow(holder, c.r, c.a) {
			t.Errorf("%s %s: %s holder outside the Privileged Groups allowed in the groups mode", c.r.Type, c.a, c.role)
		}
		for _, other := range PermissionRoles() {
			if other == c.role {
				continue
			}
			if auth.Allow(Principal{ID: "u1", Roles: []string{other}}, c.r, c.a) {
				t.Errorf("%s %s: %s alone allowed what %s grants", c.r.Type, c.a, other, c.role)
			}
		}
	}
}

func TestAuthorizer_RolesModeShortLinksNeedBothURLRoles(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(policyIn(RoleModeRoles))
	yk := Principal{ID: "u1", Groups: []string{"/UYELER/YK"}}
	for _, a := range []Action{Create, ReadMe, Read, Update, Delete} {
		someone := Resource{Type: TypeURL, OwnerID: "u2"}
		if auth.Allow(yk, someone, a) {
			t.Errorf("YK without URL roles: %s allowed", a)
		}
		both := Principal{ID: "u1", Roles: []string{RoleURLModerator, RoleURLAccess}}
		if !auth.Allow(both, someone, a) {
			t.Errorf("url:moderator + url:access: %s refused", a)
		}
	}
}

// A role held without any Group still lets the person upload what an Event
// or certificate template editor uploads: they may create one for any
// Owner team.
func TestAuthorizer_UploadRulesFollowTheRole(t *testing.T) {
	t.Parallel()
	auth := NewAuthorizer(policyIn(RoleModeRoles))
	cases := []struct {
		role string
		rule MediaUploader
	}{
		{RoleEventManage, MediaUploaderEventEditor},
		{RoleCertificateManage, MediaUploaderCertificateTemplateEditor},
	}
	for _, c := range cases {
		r := Resource{Type: TypeMedia, MediaUploader: c.rule}
		if !auth.Allow(Principal{ID: "u1", Roles: []string{c.role}}, r, Upload) {
			t.Errorf("%s holder refused the %s upload", c.role, c.rule)
		}
		if auth.Allow(Principal{ID: "u1", Groups: []string{"/UYELER/YK"}}, r, Upload) {
			t.Errorf("YK without %s allowed the %s upload in the roles mode", c.role, c.rule)
		}
		if auth.Allow(Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB"}}, r, Upload) {
			t.Errorf("a plain member allowed the %s upload", c.rule)
		}
	}
}

func TestParseRoleMode(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]RoleMode{
		"": RoleModeGroups, "groups": RoleModeGroups, " both ": RoleModeBoth, "roles": RoleModeRoles,
	} {
		got, err := ParseRoleMode(raw)
		if err != nil || got != want {
			t.Errorf("ParseRoleMode(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"role", "BOTH", "true", "group"} {
		if _, err := ParseRoleMode(raw); err == nil || !strings.Contains(err.Error(), RoleModeEnv) {
			t.Errorf("ParseRoleMode(%q) err = %v, want an error naming %s", raw, err, RoleModeEnv)
		}
	}
}

func TestAuthorizer_BothModeCountsDisagreements(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	metrics := NewRoleMetrics(log.New(&logs, "", 0), func() time.Time { return now })
	auth := NewAuthorizer(policyIn(RoleModeBoth), WithRoleMetrics(metrics))

	// A YK member whose token lacks event:manage: the group allows.
	yk := Principal{ID: "secret-sub", Client: "admin", Groups: []string{"/UYELER/YK"}}
	event := Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}
	if !auth.Allow(yk, event, Delete) || !auth.Allow(yk, event, Delete) {
		t.Fatal("both mode refused a Privileged member")
	}
	// A role holder outside the Privileged Groups: the role allows.
	holder := Principal{ID: "other-sub", Client: "admin", Roles: []string{RoleSeasonManage}}
	if !auth.Allow(holder, Resource{Type: TypeSeason}, Create) {
		t.Fatal("both mode refused a role holder")
	}
	// Group and role agree: nothing is counted.
	agree := Principal{ID: "u3", Client: "admin", Groups: []string{"/ADMIN"}, Roles: []string{RoleGroupsManage}}
	if !auth.Allow(agree, Resource{Type: TypeGroup}, Read) {
		t.Fatal("both mode refused an agreeing member")
	}
	// A member with url:access but no Privileged Group is today's normal
	// case, not a disagreement.
	if !auth.Allow(Principal{ID: "u4", Client: "admin", Roles: []string{RoleURLAccess}}, Resource{Type: TypeURL}, Create) {
		t.Fatal("url:access refused")
	}

	text := metrics.Prometheus()
	for _, want := range []string{
		`skylab_authz_role_mode{mode="both"} 1`,
		`skylab_authz_role_disagreements_total{permission="event:manage",granted_by="group",client="admin"} 2`,
		`skylab_authz_role_disagreements_total{permission="season:manage",granted_by="role",client="admin"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{`permission="groups:manage"`, `permission="url:access",granted_by="role"`} {
		if strings.Contains(text, unwanted) {
			t.Errorf("metrics count %s:\n%s", unwanted, text)
		}
	}

	// One line per permission, side and client a minute; nobody named.
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2:\n%s", len(lines), logs.String())
	}
	if !strings.Contains(lines[0], `"event":"authz_role_disagreement"`) || !strings.Contains(lines[0], `"permission":"event:manage"`) ||
		!strings.Contains(lines[0], `"granted_by":"group"`) || !strings.Contains(lines[0], `"group":"YK"`) || !strings.Contains(lines[0], `"client":"admin"`) {
		t.Errorf("log line = %s", lines[0])
	}
	if strings.Contains(logs.String(), "secret-sub") || strings.Contains(logs.String(), "other-sub") || strings.Contains(logs.String(), "/UYELER") {
		t.Errorf("log names a person or a path:\n%s", logs.String())
	}
	now = now.Add(61 * time.Second)
	auth.Allow(yk, event, Delete)
	if got := strings.Count(logs.String(), `"permission":"event:manage"`); got != 2 {
		t.Errorf("event:manage lines after a minute = %d, want 2", got)
	}
}

func TestRoleMetrics_ClientLabelIsBounded(t *testing.T) {
	t.Parallel()
	metrics := NewRoleMetrics(log.New(&bytes.Buffer{}, "", 0), time.Now)
	auth := NewAuthorizer(policyIn(RoleModeBoth), WithRoleMetrics(metrics))
	for i := range maxClientLabels + 5 {
		p := Principal{ID: "u", Client: "client-" + strings.Repeat("x", i), Groups: []string{"/ADMIN"}}
		auth.Allow(p, Resource{Type: TypeSeason}, Create)
	}
	auth.Allow(Principal{ID: "u", Groups: []string{"/ADMIN"}}, Resource{Type: TypeSeason}, Create)
	text := metrics.Prometheus()
	if got := strings.Count(text, "skylab_authz_role_disagreements_total{"); got != maxClientLabels+2 {
		t.Errorf("series = %d, want %d (the cap, other and none):\n%s", got, maxClientLabels+2, text)
	}
	for _, want := range []string{`client="other"} 5`, `client="none"} 1`} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestRoleMetrics_GroupsAndRolesModesCountNothing(t *testing.T) {
	t.Parallel()
	for _, mode := range []RoleMode{RoleModeGroups, RoleModeRoles} {
		var logs bytes.Buffer
		metrics := NewRoleMetrics(log.New(&logs, "", 0), time.Now)
		auth := NewAuthorizer(policyIn(mode), WithRoleMetrics(metrics))
		auth.Allow(Principal{ID: "u", Groups: []string{"/ADMIN"}}, Resource{Type: TypeSeason}, Create)
		auth.Allow(Principal{ID: "u", Roles: []string{RoleSeasonManage}}, Resource{Type: TypeSeason}, Create)
		text := metrics.Prometheus()
		if strings.Contains(text, "skylab_authz_role_disagreements_total{") || logs.Len() != 0 {
			t.Errorf("%s mode counted:\n%s%s", mode, text, logs.String())
		}
		if !strings.Contains(text, `skylab_authz_role_mode{mode="`+string(mode)+`"} 1`) {
			t.Errorf("%s mode gauge missing:\n%s", mode, text)
		}
	}
}

func TestRoleMetrics_NilIsQuiet(t *testing.T) {
	t.Parallel()
	var metrics *RoleMetrics
	if metrics.Prometheus() != "" {
		t.Fatal("nil metrics rendered")
	}
	auth := NewAuthorizer(policyIn(RoleModeBoth), WithRoleMetrics(nil))
	if !auth.Allow(Principal{ID: "u", Groups: []string{"/ADMIN"}}, Resource{Type: TypeSeason}, Create) {
		t.Fatal("refused")
	}
}
