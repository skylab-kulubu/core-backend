package identity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func deletePrincipal() authz.Principal {
	return authz.Principal{ID: uuid.NewString(), Groups: []string{"/ADMIN"}}
}
func deleteTree(t *testing.T) (*Memory, Group, *teamcode.MemoryLedger, *teamaudit.Memory) {
	t.Helper()
	dir := NewMemory()
	root := Group{ID: uuid.NewString(), Name: "ADMIN", Path: "/ADMIN"}
	dir.PutGroup(root)
	if err := dir.SetGroupClientRoles(context.Background(), root.ID, []ClientRole{{ClientID: "core", Role: "groups:manage"}}); err != nil {
		t.Fatal(err)
	}
	g := Group{ID: uuid.NewString(), Name: "TANISMAYEMEGIGRUBU", Path: "/TANISMAYEMEGIGRUBU"}
	dir.PutGroup(g)
	return dir, g, teamcode.NewMemoryLedger(), teamaudit.NewMemory()
}
func deleteService(dir Directory, ledger teamcode.Ledger, audit teamaudit.Writer) Service {
	return NewServiceWithOptions(AuditedDirectory(dir, audit, nil), user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), Options{TeamCodes: ledger})
}
func deleteContext(p authz.Principal) context.Context {
	return teamaudit.WithActor(context.Background(), teamaudit.Actor{ID: uuid.MustParse(p.ID), Basis: teamaudit.BasisPrivileged})
}
func deleteCode(err error) string {
	var e *GroupDeleteError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestGroupDeleteEmptyCustomAndRetry(t *testing.T) {
	dir, g, ledger, audit := deleteTree(t)
	svc, p := deleteService(dir, ledger, audit), deletePrincipal()
	if err := svc.DeleteGroup(deleteContext(p), p, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.GetGroup(context.Background(), g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("group remains: %v", err)
	}
	if ledger.Retired()[g.Name] != teamcode.ReasonDeleted {
		t.Fatal("code was not permanently retired")
	}
	rows := audit.Rows()
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionTeamDelete || rows[0].Before.Code != g.Name || rows[0].Outcome != teamaudit.OutcomeOK {
		t.Fatalf("audit: %+v", rows)
	}
	if err := svc.DeleteGroup(deleteContext(p), p, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeat DELETE: %v", err)
	}
	if _, err := svc.CreateGroup(deleteContext(p), p, "", g.Name); err == nil {
		t.Fatal("deleted code was reused")
	}
}

func TestGroupDeleteRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*Memory, *Group, *teamcode.MemoryLedger)
	}{
		{"member", "not_empty", func(d *Memory, g *Group, _ *teamcode.MemoryLedger) {
			id := uuid.New()
			d.PutUser(Person{ID: id})
			_ = d.AddMember(context.Background(), g.ID, id)
		}},
		{"child", "not_empty", func(d *Memory, g *Group, _ *teamcode.MemoryLedger) {
			d.PutGroup(Group{ID: uuid.NewString(), Name: "CHILD", Path: g.Path + "/CHILD"})
		}},
		{"owner record", "code_in_use", func(_ *Memory, g *Group, l *teamcode.MemoryLedger) { l.Hold(strings.ToLower(g.Name)) }},
		{"admin root", "privileged_root", func(_ *Memory, g *Group, _ *teamcode.MemoryLedger) { g.Name = "ADMIN"; g.Path = "/ADMIN" }},
		{"admin descendant", "privileged_root", func(_ *Memory, g *Group, _ *teamcode.MemoryLedger) { g.Path = "/ADMIN/" + g.Name }},
		{"reserved root", "group_delete_protected", func(_ *Memory, g *Group, _ *teamcode.MemoryLedger) { g.Name = "_DEFAULT"; g.Path = "/_DEFAULT" }},
		{"role leaf", "group_delete_protected", func(_ *Memory, g *Group, _ *teamcode.MemoryLedger) { g.Path = "/ROLLER/" + g.Name }},
		{"area", "group_delete_protected", func(_ *Memory, g *Group, _ *teamcode.MemoryLedger) { g.Path = "/UYELER/" + g.Name }},
		{"leader", "group_delete_protected", func(_ *Memory, g *Group, _ *teamcode.MemoryLedger) {
			g.Name = "LIDERLER"
			g.Path = "/UYELER/ARGE/WEBLAB/LIDERLER"
		}},
		{"system ancestor", "group_delete_protected", func(d *Memory, g *Group, _ *teamcode.MemoryLedger) {
			parent := Group{ID: uuid.NewString(), Name: "_DEFAULT", Path: "/_DEFAULT"}
			d.PutGroup(parent)
			g.Path = parent.Path + "/" + g.Name
		}},
		{"leader ancestor", "group_delete_protected", func(d *Memory, g *Group, _ *teamcode.MemoryLedger) {
			parent := Group{ID: uuid.NewString(), Name: "LIDERLER", Path: "/LIDERLER"}
			d.PutGroup(parent)
			g.Path = parent.Path + "/" + g.Name
		}},

		{"realm role", "protected_group", func(d *Memory, g *Group, _ *teamcode.MemoryLedger) {
			_ = d.SetGroupClientRoles(context.Background(), g.ID, []ClientRole{{Role: "USER"}})
		}},
		{"admin role", "protected_group", func(d *Memory, g *Group, _ *teamcode.MemoryLedger) {
			_ = d.SetGroupClientRoles(context.Background(), g.ID, []ClientRole{{ClientID: "core", Role: "groups:manage"}})
		}},
		{"app role", "group_delete_protected", func(d *Memory, g *Group, _ *teamcode.MemoryLedger) {
			_ = d.SetGroupClientRoles(context.Background(), g.ID, []ClientRole{{ClientID: "forms", Role: "skyforms:access"}})
		}},
		{"ancestor role", "protected_group", func(d *Memory, g *Group, _ *teamcode.MemoryLedger) {
			parent := Group{ID: uuid.NewString(), Name: "PARENT", Path: "/PARENT"}
			d.PutGroup(parent)
			_ = d.SetGroupClientRoles(context.Background(), parent.ID, []ClientRole{{Role: "USER"}})
			g.Path = parent.Path + "/" + g.Name
		}},
		{"tree mismatch", "group_tree_mismatch", func(_ *Memory, g *Group, _ *teamcode.MemoryLedger) { g.Path = "/MISSING/" + g.Name }},
		{"role visibility", "protection_unverified", func(d *Memory, _ *Group, _ *teamcode.MemoryLedger) {
			root, _ := d.GetGroup(context.Background(), "/ADMIN")
			_ = d.SetGroupClientRoles(context.Background(), root.ID, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, g, ledger, audit := deleteTree(t)
			tc.change(dir, &g, ledger)
			dir.PutGroup(g)
			dir.Ops = nil
			p := deletePrincipal()
			err := deleteService(dir, ledger, audit).DeleteGroup(deleteContext(p), p, g.ID)
			if got := deleteCode(err); got != tc.code {
				t.Fatalf("code %q, want %q (%v)", got, tc.code, err)
			}
			if slices.Contains(dir.Ops, "DeleteGroup") || len(ledger.Retired()) != 0 {
				t.Fatalf("refused deletion changed directory or codes: %v %v", dir.Ops, ledger.Retired())
			}
		})
	}
}

func TestGroupDeleteAuthorizationAndAuditBeforeMutation(t *testing.T) {
	dir, g, ledger, audit := deleteTree(t)
	svc := deleteService(dir, ledger, audit)
	for _, p := range []authz.Principal{
		{ID: uuid.NewString(), Groups: []string{"/DK"}},
		{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}},
		{ID: uuid.NewString()},
	} {
		dir.Ops = nil
		if err := svc.DeleteGroup(deleteContext(p), p, g.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("not forbidden: %v", err)
		}
		if len(dir.Ops) != 0 {
			t.Fatalf("unauthorized read/write: %v", dir.Ops)
		}
	}
	p := deletePrincipal()
	audit.SetUnavailable(true)
	if err := svc.DeleteGroup(deleteContext(p), p, g.ID); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("audit: %v", err)
	}
	if len(ledger.Retired()) != 0 || slices.Contains(dir.Ops, "DeleteGroup") {
		t.Fatal("unrecorded mutation")
	}
	for _, ref := range []string{g.Name, g.Path} {
		audit.SetUnavailable(false)
		if err := svc.DeleteGroup(deleteContext(p), p, ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("name/path %q accepted: %v", ref, err)
		}
	}
}

type betweenDeleteLedger struct {
	teamcode.Ledger
	after func()
	count int
}

func (l *betweenDeleteLedger) Locked(ctx context.Context, f func(context.Context, teamcode.Records) error) error {
	err := l.Ledger.Locked(ctx, f)
	l.count++
	if err == nil && l.count == 1 && l.after != nil {
		l.after()
	}
	return err
}
func TestGroupDeleteRechecksBetweenTransactions(t *testing.T) {
	for _, change := range []string{"member", "child", "name", "path", "reference", "role"} {
		t.Run(change, func(t *testing.T) {
			dir, g, ledger, audit := deleteTree(t)
			between := &betweenDeleteLedger{Ledger: ledger, after: func() {
				switch change {
				case "member":
					id := uuid.New()
					dir.PutUser(Person{ID: id})
					_ = dir.AddMember(context.Background(), g.ID, id)
				case "child":
					dir.PutGroup(Group{ID: uuid.NewString(), Name: "CHILD", Path: g.Path + "/CHILD"})
				case "name":
					updated := g
					updated.Name = "NEWNAME"
					dir.PutGroup(updated)
				case "path":
					updated := g
					updated.Path = "/NEWPARENT/" + g.Name
					dir.PutGroup(updated)
				case "reference":
					ledger.Hold(g.Name)
				case "role":
					_ = dir.SetGroupClientRoles(context.Background(), g.ID, []ClientRole{{ClientID: "core", Role: "groups:manage"}})
				}
			}}
			p := deletePrincipal()
			err := deleteService(dir, between, audit).DeleteGroup(deleteContext(p), p, g.ID)
			if err == nil || slices.Contains(dir.Ops, "DeleteGroup") {
				t.Fatalf("second guard allowed %s: %v", change, err)
			}
			if ledger.Retired()[g.Name] != teamcode.ReasonDeleted {
				t.Fatal("first transaction did not commit retirement")
			}
			if _, err := dir.GetGroup(context.Background(), g.ID); err != nil {
				t.Fatal("group was removed")
			}
		})
	}
}

type deleteFailureDirectory struct {
	Directory
	fail        error
	panicDelete bool
	before      func()
}

func (d *deleteFailureDirectory) DeleteGroup(ctx context.Context, id string) error {
	if d.before != nil {
		d.before()
	}
	if d.panicDelete {
		panic("controlled abort after durable retirement")
	}
	if d.fail != nil {
		return d.fail
	}
	return d.Directory.DeleteGroup(ctx, id)
}
func TestGroupDeleteDurableRetirementAndPendingAudit(t *testing.T) {
	dir, g, ledger, audit := deleteTree(t)
	failing := &deleteFailureDirectory{Directory: dir, fail: context.DeadlineExceeded, before: func() {
		if ledger.Retired()[g.Name] != teamcode.ReasonDeleted {
			t.Fatal("DELETE before durable retirement")
		}
		if rows := audit.Rows(); len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomePending {
			t.Fatal("DELETE before pending audit")
		}
	}}
	p := deletePrincipal()
	svc := deleteService(failing, ledger, audit)
	if err := svc.DeleteGroup(deleteContext(p), p, g.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	if audit.Rows()[0].Outcome != teamaudit.OutcomePending {
		t.Fatal("uncertain outcome was reported known")
	}
	failing.fail = nil
	failing.before = nil
	if err := svc.DeleteGroup(deleteContext(p), p, g.ID); err != nil {
		t.Fatalf("retired code blocked retry: %v", err)
	}
	if ledger.Retired()[g.Name] != teamcode.ReasonDeleted {
		t.Fatal("retirement disappeared")
	}
}

// These operations all use the same existing ledger. A child/member/role
// write cannot enter between the final empty check and Keycloak's DELETE.
func TestGroupDeleteSerializesCreateMemberAndRoleWrites(t *testing.T) {
	for _, write := range []string{"child", "member", "role"} {
		t.Run(write, func(t *testing.T) {
			dir, g, ledger, audit := deleteTree(t)
			entered, release := make(chan struct{}), make(chan struct{})
			blocking := &deleteFailureDirectory{Directory: dir, before: func() { close(entered); <-release }}
			attempts := make(chan struct{}, 3)
			observed := &deleteObservingLedger{Ledger: ledger, attempts: attempts}
			svc, p := deleteService(blocking, observed, audit), deletePrincipal()
			done := make(chan error, 1)
			go func() { done <- svc.DeleteGroup(deleteContext(p), p, g.ID) }()
			<-entered
			// The first two ledger turns belong to DELETE.
			<-attempts
			<-attempts
			written := make(chan error, 1)
			go func() {
				ctx := deleteContext(p)
				switch write {
				case "child":
					_, err := svc.CreateGroup(ctx, p, g.ID, "CHILD")
					written <- err
				case "member":
					id := uuid.New()
					dir.PutUser(Person{ID: id})
					written <- svc.AddMember(ctx, p, g.ID, id)
				case "role":
					written <- svc.SetGroupClientRoles(ctx, p, g.ID, []ClientRole{{ClientID: "core", Role: "url:get"}})
				}
			}()
			<-attempts
			select {
			case err := <-written:
				t.Fatalf("write passed the held delete lock: %v", err)
			default:
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("delete: %v", err)
			}
			if err := <-written; !errors.Is(err, ErrNotFound) {
				t.Fatalf("write to deleted group: %v", err)
			}
			groups, err := dir.ListGroups(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, other := range groups {
				if strings.HasPrefix(other.Path, g.Path+"/") {
					t.Fatal("orphan child created")
				}
			}
		})
	}
}

type deleteObservingLedger struct {
	teamcode.Ledger
	attempts chan struct{}
}

func (l *deleteObservingLedger) Locked(ctx context.Context, f func(context.Context, teamcode.Records) error) error {
	l.attempts <- struct{}{}
	return l.Ledger.Locked(ctx, f)
}

func TestGroupDeleteAllowsNormalTeamAncestors(t *testing.T) {
	dir, g, ledger, audit := deleteTree(t)
	for _, parent := range []Group{
		{ID: uuid.NewString(), Name: "UYELER", Path: "/UYELER"},
		{ID: uuid.NewString(), Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: uuid.NewString(), Name: "GECEKODU", Path: "/UYELER/ARGE/GECEKODU"},
	} {
		dir.PutGroup(parent)
	}
	g.Path = "/UYELER/ARGE/GECEKODU/" + g.Name
	dir.PutGroup(g)
	p := deletePrincipal()
	if err := deleteService(dir, ledger, audit).DeleteGroup(deleteContext(p), p, g.ID); err != nil {
		t.Fatalf("normal Team ancestor blocked: %v", err)
	}
}

// Keycloak can expose a root's realm roles while hiding every client
// mapping after view-clients is lost. Realm protection is no visibility proof.
type deleteHiddenClientMappings struct {
	Directory
	rootID string
}

func (d deleteHiddenClientMappings) GroupProtection(_ context.Context, id string) (Protection, error) {
	if id == d.rootID {
		return Protection{Protected: true, Reason: "realm role USER"}, nil
	}
	return Protection{}, nil
}
func (d deleteHiddenClientMappings) GroupClientRoles(context.Context, string) ([]ClientRole, error) {
	return nil, nil
}
func TestGroupDeleteCanaryRejectsRealmOnlyVisibility(t *testing.T) {
	dir, g, ledger, audit := deleteTree(t)
	root, _ := dir.GetGroup(context.Background(), "/ADMIN")
	_ = dir.SetGroupClientRoles(context.Background(), g.ID, []ClientRole{{ClientID: "core", Role: "groups:manage"}})
	p := deletePrincipal()
	hidden := deleteHiddenClientMappings{Directory: dir, rootID: root.ID}
	err := deleteService(hidden, ledger, audit).DeleteGroup(deleteContext(p), p, g.ID)
	if deleteCode(err) != CodeProtectionUnverified {
		t.Fatalf("hidden client mappings allowed deletion: %v", err)
	}
	if len(ledger.Retired()) != 0 || slices.Contains(dir.Ops, "DeleteGroup") {
		t.Fatal("hidden role-bearing Group was changed")
	}
}

func TestGroupDeleteAllowsLeaderPrefixCustomAncestor(t *testing.T) {
	dir, g, ledger, audit := deleteTree(t)
	parent := Group{ID: uuid.NewString(), Name: "LIDERLER-LAB", Path: "/LIDERLER-LAB"}
	dir.PutGroup(parent)
	g.Path = parent.Path + "/" + g.Name
	dir.PutGroup(g)
	p := deletePrincipal()
	if err := deleteService(dir, ledger, audit).DeleteGroup(deleteContext(p), p, g.ID); err != nil {
		t.Fatalf("valid custom name treated as a Leader group: %v", err)
	}
}

// A Team is deleted with the Groups under it only when they are the ones
// whose codes the first turn retired: a Sub-team added between the turns
// (its code not retired) refuses the deletion.
func TestGroupDeleteTeamSubtreeChangedBetweenTransactions(t *testing.T) {
	dir, _, ledger, audit := deleteTree(t)
	for _, g := range []Group{
		{ID: uuid.NewString(), Name: "UYELER", Path: "/UYELER"},
		{ID: uuid.NewString(), Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
	} {
		dir.PutGroup(g)
	}
	team := Group{ID: uuid.NewString(), Name: "FRONT", Path: "/UYELER/ARGE/WEBLAB/FRONT"}
	dir.PutGroup(team)
	dir.PutGroup(Group{ID: uuid.NewString(), Name: "LIDERLER", Path: team.Path + "/LIDERLER"})
	between := &betweenDeleteLedger{Ledger: ledger, after: func() {
		dir.PutGroup(Group{ID: uuid.NewString(), Name: "LATE", Path: team.Path + "/LATE"})
	}}
	p := deletePrincipal()
	err := deleteService(dir, between, audit).DeleteGroup(deleteContext(p), p, team.ID)
	if deleteCode(err) != CodeGroupTreeMismatch || slices.Contains(dir.Ops, "DeleteGroup") {
		t.Fatalf("a Sub-team added between the turns: %v %v", err, dir.Ops)
	}
	if r := ledger.Retired(); r["FRONT"] != teamcode.ReasonDeleted || r["LATE"] != "" || r["LIDERLER"] != "" {
		t.Fatalf("retired %v", r)
	}
}
