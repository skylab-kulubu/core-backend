package identity_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// flakyDirectory is a Memory whose reads of one kind fail while fail names
// it: "person" (GetUser), "groups" (GroupsForUser) or "roles"
// (EffectiveClientRoles).
type flakyDirectory struct {
	*identity.Memory
	mu   sync.Mutex
	fail string
}

func (f *flakyDirectory) failing(what string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail == what
}

func (f *flakyDirectory) set(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = what
}

func (f *flakyDirectory) GetUser(ctx context.Context, id uuid.UUID) (identity.Person, error) {
	if f.failing("person") {
		return identity.Person{}, errors.New("keycloak: unavailable")
	}
	return f.Memory.GetUser(ctx, id)
}

func (f *flakyDirectory) GroupsForUser(ctx context.Context, id uuid.UUID) ([]identity.Group, error) {
	if f.failing("groups") {
		return nil, errors.New("keycloak: unavailable")
	}
	return f.Memory.GroupsForUser(ctx, id)
}

func (f *flakyDirectory) EffectiveClientRoles(ctx context.Context, id uuid.UUID, clientID string) ([]string, error) {
	if f.failing("roles") {
		return nil, errors.New("keycloak: unavailable")
	}
	return f.Memory.EffectiveClientRoles(ctx, id, clientID)
}

// principalRealm is a realm with YK (groups:manage), DK (global:read), two
// Teams and a Protected Team (an admin role mapped to it), and its people:
// a YK member who is also in 40 other Groups (more than a token carries,
// the Group overage), a DK member, and a Leader of WEBLAB and of SECURE.
type principalRealm struct {
	dir                  *flakyDirectory
	users                *user.MemoryStore
	ids                  map[string]string
	yk, dk, leader, none uuid.UUID
}

func newPrincipalRealm(t *testing.T) principalRealm {
	t.Helper()
	ctx := context.Background()
	m := identity.NewMemory()
	r := principalRealm{dir: &flakyDirectory{Memory: m}, users: user.NewMemoryStore(), ids: map[string]string{}}
	paths := []string{"/UYELER", "/UYELER/YK", "/UYELER/DK", "/UYELER/ARGE", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER",
		"/UYELER/ARGE/MOBILAB", "/UYELER/ARGE/MOBILAB/LIDERLER", "/UYELER/ARGE/SECURE", "/UYELER/ARGE/SECURE/LIDERLER"}
	for i := range 40 {
		paths = append(paths, fmt.Sprintf("/UYELER/ARGE/WEBLAB/T%02d", i))
	}
	for _, path := range paths {
		r.ids[path] = uuid.NewString()
		m.PutGroup(identity.Group{ID: r.ids[path], Name: lastSegment(path), Path: path})
	}
	for path, role := range map[string]string{"/UYELER/YK": authz.RoleGroupsManage, "/UYELER/DK": authz.RoleGlobalRead} {
		if err := m.SetGroupClientRoles(ctx, r.ids[path], []identity.ClientRole{{ClientID: "core", Role: role}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.SetGroupClientRoles(ctx, r.ids["/UYELER/ARGE/SECURE"], []identity.ClientRole{{ClientID: "skymail", Role: "skymail:lists:read"}}); err != nil {
		t.Fatal(err)
	}
	r.yk, r.dk, r.leader, r.none = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{r.yk, r.dk, r.leader, r.none} {
		m.PutUser(identity.Person{ID: id, Email: id.String() + "@example.test"})
		if _, _, err := r.users.Upsert(ctx, user.User{ID: id, Email: id.String() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	add := func(path string, id uuid.UUID) {
		if err := m.AddMember(ctx, r.ids[path], id); err != nil {
			t.Fatal(err)
		}
	}
	add("/UYELER/YK", r.yk)
	for i := range 40 {
		add(fmt.Sprintf("/UYELER/ARGE/WEBLAB/T%02d", i), r.yk)
	}
	add("/UYELER/DK", r.dk)
	add("/UYELER/ARGE/WEBLAB/LIDERLER", r.leader)
	add("/UYELER/ARGE/SECURE/LIDERLER", r.leader)
	add("/UYELER/ARGE/MOBILAB", r.none)
	return r
}

func (r principalRealm) service(policy authz.Policy) identity.Service {
	return identity.NewServiceWithOptions(r.dir, r.users, authz.NewAuthorizer(policy), identity.Options{
		Delegation: identity.NewTeamDelegation(policy, r.dir, r.dir, identity.TeamDelegationOptions{}),
		TeamAudit:  teamaudit.NewMemory(),
	})
}

// decisionOutcome is a MembershipWriteDecision as the table compares it:
// the basis and the Team that grants it, or the refusal.
func decisionOutcome(d identity.MembershipDecision, err error) string {
	var refused *identity.DelegationError
	switch {
	case err == nil:
		return fmt.Sprintf("allow %s via %q group %s", d.Basis, d.ViaGroupID, d.Group.ID)
	case errors.As(err, &refused):
		return "refuse " + refused.Code
	case errors.Is(err, identity.ErrForbidden):
		return "refuse forbidden"
	}
	return "error " + err.Error()
}

// The Principal read from Keycloak decides a membership write as the
// person's own token does, in each role mode: Privileged writer (YK, in
// more Groups than a token carries), DK (a reader, no write), a Leader for
// their own Team, a Leader for another Team, and a Leader for a Protected
// Team.
func TestPrincipalForDecidesAsTheToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newPrincipalRealm(t)
	weblab, mobilab, secure := r.ids["/UYELER/ARGE/WEBLAB"], r.ids["/UYELER/ARGE/MOBILAB"], r.ids["/UYELER/ARGE/SECURE"]
	ykGroups := []string{"/UYELER/YK"}
	for i := range 40 {
		ykGroups = append(ykGroups, fmt.Sprintf("/UYELER/ARGE/WEBLAB/T%02d", i))
	}
	tokens := map[uuid.UUID]authz.Principal{
		r.yk:     {ID: r.yk.String(), Groups: ykGroups, Roles: []string{authz.RoleGroupsManage}, Client: "admin-panel"},
		r.dk:     {ID: r.dk.String(), Groups: []string{"/UYELER/DK"}, Roles: []string{authz.RoleGlobalRead}, Client: "admin-panel"},
		r.leader: {ID: r.leader.String(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/SECURE/LIDERLER"}, Client: "admin-panel"},
	}
	cases := []struct {
		name   string
		person uuid.UUID
		group  string
		want   string
	}{
		{"privileged writer", r.yk, weblab, "allow privileged via \"\" group " + weblab},
		{"privileged writer, protected team", r.yk, secure, "allow privileged via \"\" group " + secure},
		{"DK reads, does not write", r.dk, weblab, "refuse forbidden"},
		{"leader, own team", r.leader, weblab, "allow leader via \"" + weblab + "\" group " + weblab},
		{"leader, another team", r.leader, mobilab, "refuse forbidden"},
		{"leader, protected team", r.leader, secure, "refuse " + authz.RefusalProtectedGroup},
	}
	for _, mode := range []authz.RoleMode{authz.RoleModeGroups, authz.RoleModeBoth, authz.RoleModeRoles} {
		policy := onPolicy()
		policy.RoleMode = mode
		svc := r.service(policy)
		for _, tc := range cases {
			server, err := svc.PrincipalFor(ctx, tc.person)
			if err != nil {
				t.Fatalf("%s %s: PrincipalFor: %v", mode, tc.name, err)
			}
			token := tokens[tc.person]
			if server.ID != token.ID || server.Client != "" || server.ServiceAccount || server.Product != "" ||
				!slices.Equal(slices.Sorted(slices.Values(server.Groups)), slices.Sorted(slices.Values(token.Groups))) ||
				!slices.Equal(slices.Sorted(slices.Values(server.Roles)), slices.Sorted(slices.Values(token.Roles))) {
				t.Fatalf("%s %s: server %+v, token %+v", mode, tc.name, server, token)
			}
			fromToken := decisionOutcome(svc.MembershipWriteDecision(ctx, token, tc.group))
			fromServer := decisionOutcome(svc.MembershipWriteDecision(ctx, server, tc.group))
			if fromToken != tc.want || fromServer != tc.want {
				t.Errorf("%s %s: token %q, server %q, want %q", mode, tc.name, fromToken, fromServer, tc.want)
			}
		}
	}
}

// A Keycloak read that fails is no authority: PrincipalFor answers the
// zero Principal and ErrPrincipalUnavailable, which no membership write
// allows. A person without an active account (disabled in Keycloak,
// unknown to core, erased, unknown to Keycloak) is ErrForbidden.
func TestPrincipalForFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newPrincipalRealm(t)
	svc := r.service(onPolicy())
	weblab := r.ids["/UYELER/ARGE/WEBLAB"]

	for _, what := range []string{"person", "groups", "roles"} {
		r.dir.set(what)
		for _, id := range []uuid.UUID{r.yk, r.leader} {
			p, err := svc.PrincipalFor(ctx, id)
			if !errors.Is(err, identity.ErrPrincipalUnavailable) || p.ID != "" || len(p.Groups) != 0 || len(p.Roles) != 0 {
				t.Fatalf("%s read fails: %+v %v", what, p, err)
			}
			if got := decisionOutcome(svc.MembershipWriteDecision(ctx, p, weblab)); got != "refuse forbidden" {
				t.Fatalf("%s read fails: decided %q", what, got)
			}
		}
	}
	r.dir.set("")

	if err := r.dir.DisableUser(ctx, r.yk); err != nil {
		t.Fatal(err)
	}
	if p, err := svc.PrincipalFor(ctx, r.yk); !errors.Is(err, identity.ErrForbidden) || p.ID != "" {
		t.Fatalf("disabled: %+v %v", p, err)
	}

	stranger := uuid.New()
	r.dir.PutUser(identity.Person{ID: stranger, Email: "s@example.test"})
	if err := r.dir.AddMember(ctx, r.ids["/UYELER/YK"], stranger); err != nil {
		t.Fatal(err)
	}
	if p, err := svc.PrincipalFor(ctx, stranger); !errors.Is(err, identity.ErrForbidden) || p.ID != "" {
		t.Fatalf("no core row: %+v %v", p, err)
	}

	if _, err := r.users.RequestDeletion(ctx, r.leader, nil); err != nil {
		t.Fatal(err)
	}
	if p, err := svc.PrincipalFor(ctx, r.leader); !errors.Is(err, identity.ErrForbidden) || p.ID != "" {
		t.Fatalf("being erased: %+v %v", p, err)
	}

	ghost := uuid.New()
	if _, _, err := r.users.Upsert(ctx, user.User{ID: ghost, Email: "g@example.test"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{ghost, uuid.Nil, user.DeletedSubject} {
		if p, err := svc.PrincipalFor(ctx, id); !errors.Is(err, identity.ErrForbidden) || p.ID != "" {
			t.Fatalf("%s: %+v %v", id, p, err)
		}
	}
}
