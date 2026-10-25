package authz

import (
	"slices"
	"strings"
	"testing"
)

func TestParseAppAssignments(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{"": false, "off": false, " on ": true, "on": true} {
		got, err := ParseAppAssignments(raw)
		if err != nil || got != want {
			t.Errorf("%q: %v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"true", "ON", "1", "yes"} {
		if _, err := ParseAppAssignments(raw); err == nil || !strings.Contains(err.Error(), AppAssignmentsEnv) {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

// The panel assigns exactly core's app roles: the catalogue
// (config/app-assignments.json, ticket 03's copy once it exists) and
// AppRoles name the same (client, role) pairs (ticket 15 "Liste
// karşılaştırma testi").
func TestAppCatalogueIsAppRoles(t *testing.T) {
	t.Parallel()
	apps, err := LoadApps()
	if err != nil {
		t.Fatal(err)
	}
	var catalogue, roles []string
	for _, app := range apps {
		for _, role := range app.Roles {
			catalogue = append(catalogue, app.Client+" "+role)
		}
	}
	for _, r := range AppRoles() {
		roles = append(roles, r.Client+" "+r.Role)
	}
	slices.Sort(catalogue)
	slices.Sort(roles)
	if !slices.Equal(catalogue, roles) {
		t.Errorf("catalogue %v\nAppRoles  %v", catalogue, roles)
	}
	forms, ok := AppOf(apps, "forms")
	if !ok || !forms.AssignmentRequired || !slices.Equal(forms.SuggestedRoles, []string{"skyforms:access"}) {
		t.Errorf("forms = %+v", forms)
	}
	mail, ok := AppOf(apps, "skymail")
	if !ok || !mail.AssignmentRequired || !slices.Equal(mail.SuggestedRoles, []string{"skymail:access", "skymail:templates:read"}) {
		t.Errorf("skymail = %+v", mail)
	}
	if _, ok := AppOf(apps, "broker"); ok {
		t.Error("broker is in the catalogue")
	}
}

func TestParseAppsRefuses(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"not json":      `{`,
		"no app":        `{"apps": []}`,
		"no client":     `{"apps": [{"name": "X", "roles": ["skyforms:access"]}]}`,
		"twice":         `{"apps": [{"client": "forms", "name": "A", "roles": ["skyforms:access"]}, {"client": "forms", "name": "B", "roles": ["skyforms:access"]}]}`,
		"no name":       `{"apps": [{"client": "forms", "roles": ["skyforms:access"]}]}`,
		"no role":       `{"apps": [{"client": "forms", "name": "A", "roles": []}]}`,
		"role twice":    `{"apps": [{"client": "forms", "name": "A", "roles": ["skyforms:access", "skyforms:access"]}]}`,
		"admin role":    `{"apps": [{"client": "forms", "name": "A", "roles": ["skyforms:*"]}]}`,
		"unclassified":  `{"apps": [{"client": "skycloud", "name": "A", "roles": ["skycloud:access"]}]}`,
		"other client":  `{"apps": [{"client": "skymail", "name": "A", "roles": ["skyforms:access"]}]}`,
		"broker":        `{"apps": [{"client": "broker", "name": "A", "roles": ["read-token"]}]}`,
		"suggest other": `{"apps": [{"client": "forms", "name": "A", "roles": ["skyforms:access"], "suggestedRoles": ["skyforms:form:manage"]}]}`,
	} {
		if _, err := ParseApps([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	apps, err := ParseApps([]byte(`{"apps": [{"client": "forms", "name": "A", "roles": ["skyforms:access"]}]}`))
	if err != nil || apps[0].SuggestedRoles == nil {
		t.Errorf("%+v, %v", apps, err)
	}
}

// A panel assignment goes to a Team, a Sub-team or a Leader group; a
// Privileged root, /ROLLER and what is under them are code, /UYELER and an
// area would hand it to everyone below.
func TestAppAssignmentTarget(t *testing.T) {
	t.Parallel()
	roots, leaders := DefaultPrivilegedRoots(), DefaultPolicy().LeaderSubgroups
	for path, want := range map[string]string{
		"/UYELER/ARGE/WEBLAB":                          "",
		"/UYELER/ARGE/WEBLAB/LIDERLER":                 "",
		"/UYELER/ORGANIZASYON/ETKINLIK/KOORDINATORLER": "",
		"/UYELER/ARGE/WEBLAB/FRONT":                    "",
		"/UYELER/ARGE/WEBLAB/FRONT/LIDERLER":           "",
		"/UYELER/ARGE/YK":                              "", // a Team named YK, no root
		"/ADMIN":                                       PlacementCodeArea,
		"/UYELER/YK":                                   PlacementCodeArea,
		"/UYELER/YK/BASKAN":                            PlacementCodeArea,
		"/UYELER/DK":                                   PlacementCodeArea,
		"/ROLLER":                                      PlacementCodeArea,
		"/ROLLER/FORMS-ACCESS":                         PlacementCodeArea,
		"/UYELER":                                      PlacementMembersRoot,
		"/UYELER/ARGE":                                 PlacementArea,
		"/UYELER/ARGE/LIDERLER":                        PlacementOutside,
		"/UYELER/ARGE/WEBLAB/LIDERLER/X":               PlacementOutside,
		"/KONSOL":                                      PlacementOutside,
		"/ROLLERX/A":                                   PlacementOutside,
	} {
		if got := AppAssignmentTarget(path, roots, leaders); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
		if want != "" && PlacementDetail(want) == "" {
			t.Errorf("%s: no detail for %q", path, want)
		}
	}
	for _, reason := range []string{PlacementAdminRole, PlacementServiceAccount, PlacementInactiveUser} {
		if PlacementDetail(reason) == "" {
			t.Errorf("no detail for %q", reason)
		}
	}
}

// ADMIN and YK change an app's assignments, DK reads them, nobody else
// does either; off, nobody does anything.
func TestAllowAppAssignment(t *testing.T) {
	t.Parallel()
	on := func(mode RoleMode) Authorizer {
		policy := policyIn(mode)
		policy.AppAssignments = true
		return NewAuthorizer(policy)
	}
	r := Resource{Type: TypeAppAssignment}
	for _, tc := range []struct {
		name        string
		mode        RoleMode
		p           Principal
		read, write bool
	}{
		{"ADMIN", RoleModeGroups, Principal{ID: "u", Groups: []string{"/ADMIN"}}, true, true},
		{"YK", RoleModeGroups, Principal{ID: "u", Groups: []string{"/UYELER/YK"}}, true, true},
		{"YK duty group", RoleModeGroups, Principal{ID: "u", Groups: []string{"/UYELER/YK/BASKAN"}}, true, true},
		{"DK", RoleModeGroups, Principal{ID: "u", Groups: []string{"/UYELER/DK"}}, true, false},
		{"DK with write roles", RoleModeBoth, Principal{ID: "u", Groups: []string{"/UYELER/DK"}, Roles: []string{RoleGroupsManage}}, true, false},
		{"Leader", RoleModeGroups, Principal{ID: "u", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, false, false},
		{"member", RoleModeGroups, Principal{ID: "u", Groups: []string{"/UYELER/ARGE/WEBLAB"}}, false, false},
		{"service account", RoleModeGroups, Principal{ID: "u", Groups: []string{"/ADMIN"}, ServiceAccount: true}, false, false},
		{"product", RoleModeGroups, Principal{ID: "u", Groups: []string{"/ADMIN"}, Product: ProductForms}, false, false},
		{"no subject", RoleModeGroups, Principal{Groups: []string{"/ADMIN"}}, false, false},
		{"groups:manage in the roles mode", RoleModeRoles, Principal{ID: "u", Roles: []string{RoleGroupsManage}}, true, true},
		{"global:read in the roles mode", RoleModeRoles, Principal{ID: "u", Roles: []string{RoleGlobalRead}}, true, false},
		{"YK without roles in the roles mode", RoleModeRoles, Principal{ID: "u", Groups: []string{"/UYELER/YK"}}, false, false},
	} {
		az := on(tc.mode)
		if got := az.Allow(tc.p, r, Read); got != tc.read {
			t.Errorf("%s: read = %v", tc.name, got)
		}
		if got := az.Allow(tc.p, r, Update); got != tc.write {
			t.Errorf("%s: write = %v", tc.name, got)
		}
		if got := az.Allow(tc.p, r, Delete); got {
			t.Errorf("%s: delete = %v", tc.name, got)
		}
		c := az.Capabilities(tc.p)
		if c.Can["apps.read"] != tc.read || c.Can["apps.assign"] != tc.write {
			t.Errorf("%s: capabilities apps.read %v, apps.assign %v", tc.name, c.Can["apps.read"], c.Can["apps.assign"])
		}
	}
	off := NewAuthorizer(DefaultPolicy())
	admin := Principal{ID: "u", Groups: []string{"/ADMIN"}}
	if off.Allow(admin, r, Read) || off.Allow(admin, r, Update) {
		t.Error("off: ADMIN is allowed")
	}
	if c := off.Capabilities(admin); c.Can["apps.read"] || c.Can["apps.assign"] {
		t.Errorf("off: capabilities %v", c.Can)
	}
}
