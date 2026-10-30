package teamcode

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

func code(err error) string {
	var refused *Error
	if errors.As(err, &refused) {
		return refused.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestValid(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"WEBLAB":                       true,
		"AI-LAB":                       true,
		"AB":                           true,
		"A1":                           true,
		strings.Repeat("A", 24):        true,
		strings.Repeat("A", 25):        false,
		"A":                            false,
		"":                             false,
		"admin":                        false,
		"Weblab":                       false,
		"ADMİN":                        false, // Turkish dotted capital I
		"ADMIN​":                       false, // a zero-width space
		"АDMIN":                        false, // Cyrillic А
		"AD/MIN":                       false,
		"AD MIN":                       false,
		"AD_MIN":                       false,
		"AD~MIN":                       false,
		"AD.MIN":                       false,
		"1LAB":                         false,
		"-LAB":                         false,
		"WEBLAB\n":                     false,
		" WEBLAB":                      false,
		"_DEFAULT":                     false,
		"ŞEKER":                        false,
		"WEB" + strings.Repeat("Ü", 2): false,
	} {
		if got := Valid(name); got != want {
			t.Errorf("Valid(%q) = %v, want %v", name, got, want)
		}
	}
}

// The reserved names: the fixed ones, the roots' names, the leader
// subgroups and the Event permission table's Owner teams, without case.
func TestReserved(t *testing.T) {
	t.Parallel()
	rules := NewRules(authz.DefaultPolicy())
	for _, name := range []string{"ADMIN", "YK", "DK", "UYELER", "LIDERLER", "KOORDINATORLER", "TAKIMLAR", "_DEFAULT", "_default", "GECEKODU", "gecekodu", "admin", "ROLLER", "roller"} {
		if !rules.Reserved(name) {
			t.Errorf("%s is not reserved", name)
		}
	}
	for _, name := range []string{"WEBLAB", "ADMINS", "YKX", "LIDER"} {
		if rules.Reserved(name) {
			t.Errorf("%s is reserved", name)
		}
	}
	policy := authz.DefaultPolicy()
	policy.PrivilegedRoots = []string{"/STAFF"}
	if !NewRules(policy).Reserved("STAFF") {
		t.Error("a configured root's name is not reserved")
	}
}

// The role groups under /ROLLER are no Team and no Leader group: TeamOf
// answers nothing for them (rol-temizligi S4).
func TestRoleGroupsAreNoTeam(t *testing.T) {
	t.Parallel()
	policy := authz.DefaultPolicy()
	for _, path := range []string{"/ROLLER", "/ROLLER/FORMS-ACCESS", "/ROLLER/FORMS-ACCESS/LIDERLER", "/ROLLER/A/B/C"} {
		if team, role, ok := TeamOf(path, policy.PrivilegedRoots, policy.LeaderSubgroups); ok || team != "" || role != "" {
			t.Errorf("TeamOf(%s) = %q, %q, %v", path, team, role, ok)
		}
	}
}

func TestCreate(t *testing.T) {
	t.Parallel()
	rules := NewRules(authz.DefaultPolicy())
	for _, tc := range []struct {
		parent, name string
		want         string
		scope        Scope
	}{
		{"/UYELER/ARGE", "WEBLAB", "", Everywhere},
		{"/UYELER/ARGE/ALGOLAB", "AGC", "", Everywhere},
		{"/UYELER/YK", "SEKRETER", "", Everywhere},
		{"", "KULUP-DISI", "", Everywhere},
		// Not a code.
		{"/UYELER/ARGE", "admin", InvalidName, Everywhere},
		{"/UYELER/ARGE", "ADMİN", InvalidName, Everywhere},
		{"/UYELER/ARGE", "AD/MIN", InvalidName, Everywhere},
		{"/UYELER/ARGE", "A", InvalidName, Everywhere},
		{"/UYELER/ARGE", strings.Repeat("W", 25), InvalidName, Everywhere},
		{"/UYELER/ARGE", "Web Lab", InvalidName, Everywhere},
		// A root's name only at a root.
		{"", "ADMIN", "", AtPath},
		{"", "YK", "", AtPath},
		{"", "DK", "", AtPath},
		{"/UYELER", "ADMIN", "", AtPath},
		{"/UYELER", "YK", "", AtPath},
		{"/UYELER", "DK", "", AtPath},
		{"/UYELER/ARGE/WEBLAB", "ADMIN", ReservedName, Everywhere},
		{"/UYELER/ARGE", "YK", ReservedName, Everywhere},
		{"/UYELER/ORGANIZASYON", "DK", ReservedName, Everywhere},
		{"/UYELER/YK", "ADMIN", ReservedName, Everywhere},
		{"/ADMIN", "ADMIN", ReservedName, Everywhere},
		{"/A", "ADMIN", ReservedName, Everywhere},
		// A leader subgroup's name only right under a Team.
		{"/UYELER/ARGE/WEBLAB", "LIDERLER", "", AtPath},
		{"/UYELER/ORGANIZASYON/ARTLAB", "KOORDINATORLER", "", AtPath},
		{"/UYELER/ARGE/ALGOLAB/AGC", "LIDERLER", "", AtPath},
		{"/UYELER/ARGE", "LIDERLER", ReservedName, Everywhere},
		{"/UYELER", "LIDERLER", ReservedName, Everywhere},
		{"", "LIDERLER", ReservedName, Everywhere},
		{"/UYELER/ARGE/WEBLAB/LIDERLER", "LIDERLER", ReservedName, Everywhere},
		{"/UYELER/YK", "LIDERLER", ReservedName, Everywhere},
		{"/UYELER/YK/BASKAN", "LIDERLER", ReservedName, Everywhere},
		{"/DIS/ARGE/WEBLAB", "LIDERLER", ReservedName, Everywhere},
		// ROLLER only at /ROLLER (rol-temizligi S4): no Team named so.
		{"", "ROLLER", "", AtPath},
		{"/UYELER/ARGE", "ROLLER", ReservedName, Everywhere},
		{"/UYELER/ARGE/WEBLAB", "ROLLER", ReservedName, Everywhere},
		{"/ROLLER", "ROLLER", ReservedName, Everywhere},
		{"/ROLLER", "LIDERLER", ReservedName, Everywhere},
		{"/ROLLER", "FORMS-ACCESS", "", Everywhere},
		// The others never.
		{"", "UYELER", ReservedName, Everywhere},
		{"/UYELER/ARGE", "UYELER", ReservedName, Everywhere},
		{"/UYELER", "TAKIMLAR", ReservedName, Everywhere},
		{"/UYELER/ARGE/WEBLAB", "GECEKODU", ReservedName, Everywhere},
	} {
		scope, err := rules.Create(tc.parent, tc.name)
		if code(err) != tc.want || (err == nil && scope != tc.scope) {
			t.Errorf("Create(%q, %q) = %v, %v; want %q, %v", tc.parent, tc.name, scope, err, tc.want, tc.scope)
		}
	}
}

func TestRename(t *testing.T) {
	t.Parallel()
	rules := NewRules(authz.DefaultPolicy())
	for _, tc := range []struct {
		path, name string
		leaf       bool
		want       string
	}{
		{"/UYELER/ARGE/WEBLAB", "WEB", false, ""},
		{"/UYELER/YK/BASKAN", "BASKAN-YRD", true, ""},
		{"/UYELER/ARGE/WEBLAB/LIDERLER", "KOORDINATORLER", true, ""},
		{"/UYELER/ARGE/WEBLAB/ESKI", "LIDERLER", true, ""},
		// A leader subgroup is a leaf.
		{"/UYELER/ARGE/WEBLAB/ESKI", "LIDERLER", false, ReservedName},
		{"/UYELER/ARGE/WEBLAB", "admin", false, InvalidName},
		{"/UYELER/ARGE/WEBLAB", "ADMIN", false, ReservedName},
		{"/UYELER/ARGE/WEBLAB/X", "YK", true, ReservedName},
		// Renames never move or make a root, nor what is above one.
		{"/ADMIN", "ADMINS", false, PrivilegedRoot},
		{"/UYELER/YK", "YK-ESKI", false, PrivilegedRoot},
		{"/UYELER/DK", "DK", false, PrivilegedRoot},
		{"/UYELER", "MEMBERS", false, PrivilegedRoot},
		{"/UYELER/FOO", "ADMIN", true, PrivilegedRoot},
		{"/UYELER/FOO", "YK", true, PrivilegedRoot},
		{"/FOO", "DK", true, PrivilegedRoot},
	} {
		_, err := rules.Rename(tc.path, tc.name, tc.leaf)
		if code(err) != tc.want {
			t.Errorf("Rename(%q, %q, leaf %v) = %v; want %q", tc.path, tc.name, tc.leaf, err, tc.want)
		}
	}
	// A configured root above a Group: renaming the Group above it moves it.
	policy := authz.DefaultPolicy()
	policy.PrivilegedRoots = []string{"/UYELER/ARGE/SKYSEC"}
	custom := NewRules(policy)
	if _, err := custom.Rename("/UYELER/ARGE", "ARGE-X", false); code(err) != PrivilegedRoot {
		t.Errorf("renaming above a configured root: %v", err)
	}
	if _, err := custom.Rename("/UYELER/YK", "YK-ESKI", false); err != nil {
		t.Errorf("YK is no root here: %v", err)
	}
}

func TestTeamOf(t *testing.T) {
	t.Parallel()
	roots, leaders := authz.DefaultPrivilegedRoots(), authz.DefaultPolicy().LeaderSubgroups
	for _, tc := range []struct {
		path, team, role string
		ok               bool
	}{
		{"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB", "", true},
		{"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/WEBLAB", "LIDERLER", true},
		{"/UYELER/ARGE/ALGOLAB/AGC", "/UYELER/ARGE/ALGOLAB/AGC", "", true},
		{"/UYELER/ARGE/WEBLAB/ADMIN", "/UYELER/ARGE/WEBLAB/ADMIN", "", true},
		{"/UYELER/ARGE", "", "", false},
		{"/UYELER/ARGE/LIDERLER", "", "", false},
		{"/UYELER/YK/BASKAN", "", "", false},
		{"/UYELER/ARGE/WEBLAB/LIDERLER/X", "", "", false},
		{"/UYELER", "", "", false},
		{"/UYELERX/ARGE/WEBLAB", "", "", false},
		{"/UYELER/ARGE/WEBLAB/", "", "", false},
	} {
		team, role, ok := TeamOf(tc.path, roots, leaders)
		if team != tc.team || role != tc.role || ok != tc.ok {
			t.Errorf("TeamOf(%q) = %q %q %v; want %q %q %v", tc.path, team, role, ok, tc.team, tc.role, tc.ok)
		}
	}
}

func TestMemoryLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ledger := NewMemoryLedger()
	ledger.Hold("WEBLAB")
	held := func(code string) bool {
		var got bool
		if err := ledger.Locked(ctx, func(ctx context.Context, records Records) error {
			var err error
			got, err = records.Held(ctx, code)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if !held("weblab") || held("GAMELAB") {
		t.Fatal("held codes")
	}
	ledger.HoldAs(RecordCertificate, "CERTLAB")
	references := func(code string) []string {
		var got []string
		if err := ledger.Locked(ctx, func(ctx context.Context, records Records) error {
			var err error
			got, err = records.References(ctx, code)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := references("weblab"); !slices.Equal(got, []string{RecordEvent}) {
		t.Fatalf("References(weblab) = %v", got)
	}
	if got := references("CERTLAB"); !slices.Equal(got, []string{RecordCertificate}) {
		t.Fatalf("References(CERTLAB) = %v", got)
	}
	if got := references("GAMELAB"); got != nil {
		t.Fatalf("References(GAMELAB) = %v", got)
	}
	// A write that fails retires nothing.
	failed := errors.New("keycloak down")
	if err := ledger.Locked(ctx, func(ctx context.Context, records Records) error {
		if err := records.Retire(ctx, "GAMELAB", "g", ReasonRenamed); err != nil {
			return err
		}
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("Locked = %v", err)
	}
	if held("GAMELAB") {
		t.Fatal("a failed write retired its code")
	}
	if err := ledger.Locked(ctx, func(ctx context.Context, records Records) error {
		return records.Retire(ctx, "GAMELAB", "g", ReasonRenamed)
	}); err != nil {
		t.Fatal(err)
	}
	if !held("gamelab") || ledger.Retired()["GAMELAB"] != ReasonRenamed {
		t.Fatalf("retired %v", ledger.Retired())
	}
}
