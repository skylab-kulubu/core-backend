package identity_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// namesTree is a realm like production's: the roots, two areas, teams, a
// Sub-team, a Leader group and a Group named before the rule.
func namesTree(t *testing.T) (*identity.Memory, *teamcode.MemoryLedger, identity.Service) {
	t.Helper()
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-admin", Name: "ADMIN", Path: "/ADMIN"},
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
		{ID: "g-baskan", Name: "BASKAN", Path: "/UYELER/YK/BASKAN"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-orga", Name: "ORGANIZASYON", Path: "/UYELER/ORGANIZASYON"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "g-weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{ID: "g-skysec", Name: "SKYSEC", Path: "/UYELER/ARGE/SKYSEC"},
		{ID: "g-algolab", Name: "ALGOLAB", Path: "/UYELER/ARGE/ALGOLAB"},
		{ID: "g-agc", Name: "AGC", Path: "/UYELER/ARGE/ALGOLAB/AGC"},
		{ID: "g-gecekodu", Name: "GECEKODU", Path: "/UYELER/ORGANIZASYON/GECEKODU"},
		{ID: "g-old", Name: "Eski Editörler", Path: "/UYELER/Eski Editörler"},
		{ID: "g-lower", Name: "artlab", Path: "/UYELER/ORGANIZASYON/artlab"},
	} {
		dir.PutGroup(g)
	}
	ledger := teamcode.NewMemoryLedger()
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{TeamCodes: ledger})
	return dir, ledger, svc
}

func refusal(err error) string {
	var refused *teamcode.Error
	if errors.As(err, &refused) {
		return refused.Code
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return ""
}

// Every create passes the Team code rule, Privileged callers too. The
// attacks a Leader who may create Groups would try (ticket 01) are among
// them: a Privileged name under a team, another team's code, a deleted
// team's code, a Leader group where none belongs.
func TestService_CreateGroupNameRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, parent, group string
		held                []string
		want                string
	}{
		{name: "a new team", parent: "g-arge", group: "NEWLAB"},
		{name: "a Sub-team", parent: "g-agc", group: "AGC-ALT"},
		{name: "a Group under a root", parent: "g-yk", group: "SEKRETER"},
		{name: "not a code", parent: "g-arge", group: "newlab", want: teamcode.InvalidName},
		{name: "a Turkish letter", parent: "g-arge", group: "ADMİN", want: teamcode.InvalidName},
		{name: "a path separator", parent: "g-arge", group: "AD/MIN", want: teamcode.InvalidName},
		{name: "one letter", parent: "g-arge", group: "A", want: teamcode.InvalidName},
		{name: "25 characters", parent: "g-arge", group: "ABCDEFGHIJKLMNOPQRSTUVWXY", want: teamcode.InvalidName},
		{name: "ADMIN under a team", parent: "g-weblab", group: "ADMIN", want: teamcode.ReservedName},
		{name: "YK under an area", parent: "g-arge", group: "YK", want: teamcode.ReservedName},
		{name: "DK under a Sub-team", parent: "g-agc", group: "DK", want: teamcode.ReservedName},
		{name: "LIDERLER under an area", parent: "g-arge", group: "LIDERLER", want: teamcode.ReservedName},
		{name: "LIDERLER under a root", parent: "g-yk", group: "LIDERLER", want: teamcode.ReservedName},
		{name: "a second LIDERLER", parent: "g-weblab", group: "LIDERLER", want: teamcode.NameTaken},
		{name: "a policy's Owner team", parent: "g-weblab", group: "GECEKODU", want: teamcode.ReservedName},
		{name: "UYELER again", parent: "g-arge", group: "UYELER", want: teamcode.ReservedName},
		{name: "another team's code", parent: "g-weblab", group: "SKYSEC", want: teamcode.NameTaken},
		{name: "another Sub-team's code", parent: "g-weblab", group: "AGC", want: teamcode.NameTaken},
		{name: "a root's subgroup's code", parent: "g-weblab", group: "BASKAN", want: teamcode.NameTaken},
		{name: "a code taken in another case", parent: "g-arge", group: "ARTLAB", want: teamcode.NameTaken},
		{name: "a code only a record holds", parent: "g-weblab", group: "AIRLAB", held: []string{"airlab"}, want: teamcode.NameTaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, ledger, svc := namesTree(t)
			ledger.Hold(tc.held...)
			dir.Ops = nil
			created, err := svc.CreateGroup(context.Background(), privileged(), tc.parent, tc.group)
			if got := refusal(err); got != tc.want {
				t.Fatalf("CreateGroup(%s) = %+v, %v; want %q", tc.group, created, err, tc.want)
			}
			if tc.want != "" && slices.Contains(dir.Ops, "CreateGroup") {
				t.Fatalf("a refused name reached the directory: %v", dir.Ops)
			}
			if tc.want == "" && created.Name != tc.group {
				t.Fatalf("created %+v", created)
			}
		})
	}
}

// A root's name at a root, and a Leader group under a Team without one:
// the path alone must be free, so /UYELER/ADMIN is created beside /ADMIN.
func TestService_CreateGroupReservedNamesAtTheirPlace(t *testing.T) {
	t.Parallel()
	dir, _, svc := namesTree(t)
	ctx := context.Background()
	for _, tc := range []struct{ parent, name, path string }{
		{"g-uyeler", "ADMIN", "/UYELER/ADMIN"},
		{"", "YK", "/YK"},
		{"g-skysec", "LIDERLER", "/UYELER/ARGE/SKYSEC/LIDERLER"},
		{"g-agc", "KOORDINATORLER", "/UYELER/ARGE/ALGOLAB/AGC/KOORDINATORLER"},
	} {
		created, err := svc.CreateGroup(ctx, privileged(), tc.parent, tc.name)
		if err != nil || created.Path != tc.path {
			t.Fatalf("CreateGroup(%s, %s) = %+v, %v", tc.parent, tc.name, created, err)
		}
	}
	// A root that exists is taken.
	if _, err := svc.CreateGroup(ctx, privileged(), "", "ADMIN"); refusal(err) != teamcode.NameTaken {
		t.Fatalf("a second /ADMIN: %v", err)
	}
	_ = dir
}

func TestService_RenameGroupNameRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, group, to string
		held            []string
		want            string
	}{
		{name: "a new code", group: "g-weblab", to: "WEB"},
		{name: "under a root", group: "g-baskan", to: "BASKAN-YRD"},
		{name: "not a code", group: "g-weblab", to: "web", want: teamcode.InvalidName},
		{name: "ADMIN for a team", group: "g-weblab", to: "ADMIN", want: teamcode.ReservedName},
		{name: "a Sub-team to its team's LIDERLER", group: "g-agc", to: "LIDERLER"},
		{name: "a team with a Sub-team to LIDERLER", group: "g-algolab", to: "LIDERLER", want: teamcode.ReservedName},
		{name: "another team's code", group: "g-weblab", to: "SKYSEC", want: teamcode.NameTaken},
		{name: "a code a record holds", group: "g-weblab", to: "AIRLAB", held: []string{"AIRLAB"}, want: teamcode.NameTaken},
		{name: "a root", group: "g-yk", to: "YK-ESKI", want: teamcode.PrivilegedRoot},
		{name: "above the roots", group: "g-uyeler", to: "MEMBERS", want: teamcode.PrivilegedRoot},
		{name: "into a root", group: "g-orga", to: "DK", want: teamcode.PrivilegedRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, ledger, svc := namesTree(t)
			ledger.Hold(tc.held...)
			dir.Ops = nil
			before, err := dir.GetGroup(context.Background(), tc.group)
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.UpdateGroup(context.Background(), privileged(), tc.group, tc.to, nil)
			if got := refusal(err); got != tc.want {
				t.Fatalf("rename %s to %s: %v; want %q", before.Name, tc.to, err, tc.want)
			}
			after, _ := dir.GetGroup(context.Background(), tc.group)
			retired := ledger.Retired()
			if tc.want != "" {
				if after.Name != before.Name || len(retired) != 0 || slices.Contains(dir.Ops, "UpdateGroup") {
					t.Fatalf("a refused rename changed something: %+v, retired %v, ops %v", after, retired, dir.Ops)
				}
				return
			}
			if after.Name != tc.to {
				t.Fatalf("renamed %+v", after)
			}
			if _, ok := retired[before.Name]; !ok {
				t.Fatalf("the old code %s is not retired: %v", before.Name, retired)
			}
		})
	}
}

// What a rename keeps: the same name (blanks aside) is no rename, even for
// a name the rule would refuse today; a change of case keeps the code and
// retires nothing; a reserved name given up (a Leader group renamed) is not
// recorded as retired.
func TestService_RenameKeepsWhatIsNotANewCode(t *testing.T) {
	t.Parallel()
	dir, ledger, svc := namesTree(t)
	ctx := context.Background()
	updated, err := svc.UpdateGroup(ctx, privileged(), "g-old", " Eski Editörler ", map[string]string{"public_listing": "false"})
	if err != nil || updated.Name != "Eski Editörler" || updated.Attributes["public_listing"] != "false" {
		t.Fatalf("attribute change on an old name: %+v, %v", updated, err)
	}
	if _, err := svc.UpdateGroup(ctx, privileged(), "g-lower", "ARTLAB", nil); err != nil {
		t.Fatalf("change of case: %v", err)
	}
	if _, err := svc.UpdateGroup(ctx, privileged(), "g-weblab-l", "KOORDINATORLER", nil); err != nil {
		t.Fatalf("Leader group to KOORDINATORLER: %v", err)
	}
	if retired := ledger.Retired(); len(retired) != 0 {
		t.Fatalf("retired %v", retired)
	}
	// A Group renamed back to its retired code: the code is nobody's.
	if _, err := svc.UpdateGroup(ctx, privileged(), "g-skysec", "SKYSEC-X", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateGroup(ctx, privileged(), "g-skysec", "SKYSEC", nil); refusal(err) != teamcode.NameTaken {
		t.Fatalf("back to a retired code: %v", err)
	}
	_ = dir
}

// A member creates and renames nothing: the rule comes after the decision.
func TestService_GroupNamesAfterTheDecision(t *testing.T) {
	t.Parallel()
	_, _, svc := namesTree(t)
	ctx := context.Background()
	if _, err := svc.CreateGroup(ctx, member(), "g-arge", "admin"); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("member create: %v", err)
	}
	if _, err := svc.UpdateGroup(ctx, member(), "g-weblab", "ADMIN", nil); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("member rename: %v", err)
	}
	// A member of a Group named ADMIN under a team is no Privileged caller.
	fake := authz.Principal{ID: "fake", Groups: []string{"/UYELER/ARGE/WEBLAB/ADMIN"}}
	if _, err := svc.CreateGroup(ctx, fake, "g-arge", "NEWLAB"); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("fake ADMIN create: %v", err)
	}
}
