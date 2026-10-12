package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// delegationGroups is a realm like production's, with Keycloak ids: two
// areas, WEBLAB with a Sub-team FRONT, MOBILAB, a protected Team and the YK
// root.
var delegationGroups = []identity.Group{
	{Name: "UYELER", Path: "/UYELER"},
	{Name: "ARGE", Path: "/UYELER/ARGE"},
	{Name: "ORGANIZASYON", Path: "/UYELER/ORGANIZASYON"},
	{Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
	{Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
	{Name: "KOORDINATORLER", Path: "/UYELER/ARGE/WEBLAB/KOORDINATORLER"},
	{Name: "FRONT", Path: "/UYELER/ARGE/WEBLAB/FRONT"},
	{Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER"},
	{Name: "MOBILAB", Path: "/UYELER/ARGE/MOBILAB"},
	{Name: "LIDERLER", Path: "/UYELER/ARGE/MOBILAB/LIDERLER"},
	{Name: "SECURE", Path: "/UYELER/ARGE/WEBLAB/SECURE"},
	{Name: "YK", Path: "/UYELER/YK"},
}

// heardChanges is a membership notifier (the team membership mail) that
// wants every Group and keeps what it hears.
type heardChanges struct {
	mu      sync.Mutex
	changes []identity.MembershipChange
}

func (h *heardChanges) Notifies(identity.Group) bool { return true }
func (h *heardChanges) PrecheckFailed()              {}
func (h *heardChanges) MembershipChanged(_ context.Context, change identity.MembershipChange) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes = append(h.changes, change)
}

func (h *heardChanges) all() []identity.MembershipChange {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.changes)
}

type delegationEnv struct {
	t      *testing.T
	heard  *heardChanges
	app    *fiber.App
	keys   *testauth.Bundle
	dir    *identity.Memory
	users  *user.MemoryStore
	audit  *teamaudit.Memory
	ids    map[string]string    // Group path → id
	people map[string]uuid.UUID // name → subject
}

type delegationSetup struct {
	on        bool
	none      bool // no TeamDelegation at all: the release before ticket 04
	writes    int
	groupsCap int
	searches  int
}

func newDelegationEnv(t *testing.T, setup delegationSetup) *delegationEnv {
	t.Helper()
	e := &delegationEnv{
		t: t, keys: testauth.New(t), dir: identity.NewMemory(), users: user.NewMemoryStore(), audit: teamaudit.NewMemory(), heard: &heardChanges{},
		ids: map[string]string{}, people: map[string]uuid.UUID{},
	}
	for _, g := range delegationGroups {
		g.ID = uuid.NewString()
		e.ids[g.Path] = g.ID
		e.dir.PutGroup(g)
	}
	// YK, a Privileged root, maps core's roles (ADR-0059): the canary
	// protection read core checks before any delegated write.
	if err := e.dir.SetGroupClientRoles(context.Background(), e.ids["/UYELER/YK"], []identity.ClientRole{{ClientID: "core", Role: authz.RoleGroupsManage}}); err != nil {
		t.Fatal(err)
	}
	// SECURE maps one of core's permission roles: a protected Group.
	if err := e.dir.SetGroupClientRoles(context.Background(), e.ids["/UYELER/ARGE/WEBLAB/SECURE"], []identity.ClientRole{{ClientID: "core", Role: authz.RoleEventManage}}); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"webLeader":   "/UYELER/ARGE/WEBLAB/LIDERLER",
		"webCoord":    "/UYELER/ARGE/WEBLAB/KOORDINATORLER",
		"frontLeader": "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER",
		"mobLeader":   "/UYELER/ARGE/MOBILAB/LIDERLER",
		"member":      "/UYELER/ARGE/WEBLAB",
		"frontMember": "/UYELER/ARGE/WEBLAB/FRONT",
		"yk":          "/UYELER/YK",
		"outsider":    "",
		"newcomer":    "",
		"exLeader":    "",
	} {
		e.person(name, path)
	}
	policy := authz.DefaultPolicy()
	policy.TeamDelegation = setup.on
	az := authz.NewAuthorizer(policy)
	options := identity.Options{TeamCodes: teamcode.NewMemoryLedger(), TeamAudit: e.audit, Membership: e.heard}
	var delegation *identity.TeamDelegation
	if !setup.none {
		delegation = identity.NewTeamDelegation(policy, e.dir, e.dir, identity.TeamDelegationOptions{
			WritesPerMinute: setup.writes, GroupLimit: setup.groupsCap, SearchesPerMinute: setup.searches,
		})
		options.Delegation = delegation
	}
	deps := memoryDepsDecidedBy(e.dir, options, az)
	deps.Identity = identity.NewServiceWithOptions(identity.AuditedDirectory(e.dir, e.audit, nil), e.users, az, options)
	deps.ParseToken = e.keys.Parse()
	deps.ManagedTeams = delegation
	e.app = httpx.New(deps)
	return e
}

// person puts a person in the directory and in core's users, directly in
// the Group at path ("" for none).
func (e *delegationEnv) person(name, path string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.people[name] = id
	email := strings.ToLower(name) + "@example.test"
	e.dir.PutUser(identity.Person{ID: id, Email: email, FirstName: name, LastName: "Kişi", Username: "u-" + name, SkyNumber: "SKY-" + name})
	if _, _, err := e.users.Upsert(context.Background(), user.User{ID: id, Email: email, FirstName: name, LastName: "Kişi", Username: "u-" + name}); err != nil {
		e.t.Fatal(err)
	}
	if path != "" {
		if err := e.dir.AddMember(context.Background(), e.ids[path], id); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

// token is a real signed token of the person: the groups claim says what
// their direct Groups are in the directory, or groups when given (a token
// issued before a change).
func (e *delegationEnv) token(name string, groups ...string) string {
	e.t.Helper()
	if groups == nil {
		mine, err := e.dir.GroupsForUser(context.Background(), e.people[name])
		if err != nil {
			e.t.Fatal(err)
		}
		groups = identity.GroupPaths(mine)
	}
	return e.keys.Token(e.t, jwt.MapClaims{
		"sub": e.people[name].String(), "email": name + "@example.test", "given_name": name, "family_name": "K", "groups": groups,
	})
}

// queryOf is the "?query" of path, "" without one.
func queryOf(path string) string {
	if _, query, ok := strings.Cut(path, "?"); ok {
		return "?" + query
	}
	return ""
}

type rawResponse struct {
	status int
	body   []byte
}

func (r rawResponse) problem() map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(r.body, &out)
	return out
}

func (e *delegationEnv) send(token, method, path, body string) rawResponse {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return rawResponse{status: resp.StatusCode, body: raw}
}

func (e *delegationEnv) group(path string) string { return "/v1/groups/" + e.ids[path] }

func (e *delegationEnv) add(as, path, who string) rawResponse {
	return e.send(e.token(as), fiber.MethodPost, e.group(path)+"/members", `{"userId":"`+e.people[who].String()+`"}`)
}

func (e *delegationEnv) remove(as, path, who string) rawResponse {
	return e.send(e.token(as), fiber.MethodDelete, e.group(path)+"/members/"+e.people[who].String(), "")
}

func (e *delegationEnv) directlyIn(who, path string) bool {
	groups, err := e.dir.GroupsForUser(context.Background(), e.people[who])
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.ContainsFunc(groups, func(g identity.Group) bool { return g.Path == path })
}

func wantStatus(t *testing.T, name string, got rawResponse, status int, code string) {
	t.Helper()
	if got.status != status {
		t.Errorf("%s: status %d (%s), want %d", name, got.status, got.body, status)
		return
	}
	if code != "" || status == fiber.StatusForbidden {
		if have, _ := got.problem()["code"].(string); have != code {
			t.Errorf("%s: code %q (%s), want %q", name, have, got.body, code)
		}
	}
}

// Every delegated route, with delegation off, answers a Leader exactly as
// the release before ticket 04 does, reads nothing for it and writes no
// row; Privileged is answered as before.
func TestTeamDelegationOffChangesNothing(t *testing.T) {
	t.Parallel()
	off := newDelegationEnv(t, delegationSetup{on: false})
	before := newDelegationEnv(t, delegationSetup{none: true})
	type request struct{ as, method, path, body string }
	requests := func(e *delegationEnv) []request {
		web, front := e.ids["/UYELER/ARGE/WEBLAB"], e.ids["/UYELER/ARGE/WEBLAB/FRONT"]
		out := []request{}
		for _, as := range []string{"webLeader", "webCoord", "frontLeader", "member", "outsider"} {
			out = append(out,
				request{as, fiber.MethodGet, "/v1/groups/" + web, ""},
				request{as, fiber.MethodGet, "/v1/groups/" + front + "/members", ""},
				request{as, fiber.MethodPost, "/v1/groups/" + front + "/members", `{"userId":"` + e.people["newcomer"].String() + `"}`},
				request{as, fiber.MethodDelete, "/v1/groups/" + front + "/members/" + e.people["frontMember"].String(), ""},
				request{as, fiber.MethodGet, "/v1/groups/" + web + "/member-candidates?q=newcomer", ""},
				request{as, fiber.MethodGet, "/v1/groups", ""},
				request{as, fiber.MethodPost, "/v1/groups", `{"name":"NEWLAB","parentId":"` + web + `"}`},
				request{as, fiber.MethodPatch, "/v1/groups/" + front, `{"attributes":{"display_name_tr":"Ön"}}`},
				request{as, fiber.MethodGet, "/v1/users/me/managed-groups", ""},
			)
		}
		return out
	}
	offRequests, beforeRequests := requests(off), requests(before)
	for i := range offRequests {
		r, b := offRequests[i], beforeRequests[i]
		token := off.token(r.as)
		opsBefore := len(off.dir.Ops)
		got := off.send(token, r.method, r.path, r.body)
		want := before.send(before.token(b.as), b.method, b.path, b.body)
		if r.path == "/v1/users/me/managed-groups" {
			// A new route: before the ticket it did not exist (404); off,
			// it answers nobody's Teams.
			if got.status != fiber.StatusOK || string(bytes.TrimSpace(got.body)) != "[]" {
				t.Errorf("%s %s %s: %d %s", r.as, r.method, r.path, got.status, got.body)
			}
			continue
		}
		if strings.Contains(r.path, "member-candidates") {
			// A new route: off, a Leader is refused as on every Group route.
			want = before.send(before.token(b.as), fiber.MethodGet, "/v1/groups/"+before.ids["/UYELER/ARGE/WEBLAB"], "")
		}
		// The same problem but for the instance, which names the request's
		// ids (each app has its own).
		gotProblem, wantProblem := got.problem(), want.problem()
		if gotProblem["instance"] != strings.SplitN(r.path, "?", 2)[0]+queryOf(r.path) {
			t.Errorf("%s %s %s: instance %v", r.as, r.method, r.path, gotProblem["instance"])
		}
		delete(gotProblem, "instance")
		delete(wantProblem, "instance")
		gotJSON, _ := json.Marshal(gotProblem)
		wantJSON, _ := json.Marshal(wantProblem)
		if got.status != want.status || !bytes.Equal(gotJSON, wantJSON) {
			t.Errorf("%s %s %s: off %d %s; before %d %s", r.as, r.method, r.path, got.status, got.body, want.status, want.body)
		}
		if got.status != fiber.StatusForbidden {
			t.Errorf("%s %s %s: %d, want today's 403", r.as, r.method, r.path, got.status)
		}
		if ops := off.dir.Ops[opsBefore:]; len(ops) != 0 {
			t.Errorf("%s %s %s: read %v for a refused caller", r.as, r.method, r.path, ops)
		}
	}
	if rows := off.audit.Rows(); len(rows) != 0 {
		t.Fatalf("off wrote audit rows: %+v", rows)
	}
	if !off.directlyIn("frontMember", "/UYELER/ARGE/WEBLAB/FRONT") || off.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB/FRONT") {
		t.Fatal("off changed a membership")
	}
	// The capabilities answer has the new field, empty for everyone.
	for _, as := range []string{"webLeader", "yk"} {
		got := off.send(off.token(as), fiber.MethodGet, "/v1/users/me/capabilities", "")
		var answer struct {
			ManagedTeams []any          `json:"managedTeams"`
			Can          map[string]any `json:"can"`
		}
		if err := json.Unmarshal(got.body, &answer); err != nil || got.status != fiber.StatusOK || answer.ManagedTeams == nil || len(answer.ManagedTeams) != 0 {
			t.Errorf("%s capabilities: %d %s", as, got.status, got.body)
		}
	}
	// Privileged: the same answers and writes as before.
	for _, e := range []*delegationEnv{off, before} {
		yk := e.token("yk")
		if got := e.send(yk, fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB/FRONT")+"/members", ""); got.status != fiber.StatusOK || !strings.Contains(string(got.body), "u-frontMember") {
			t.Errorf("privileged roster: %d %s", got.status, got.body)
		}
		if got := e.add("yk", "/UYELER/ARGE/WEBLAB/LIDERLER", "newcomer"); got.status != fiber.StatusNoContent {
			t.Errorf("privileged add: %d %s", got.status, got.body)
		}
	}
	if rows := off.audit.Rows(); len(rows) != 1 || rows[0].Actor.Basis != teamaudit.BasisPrivileged || rows[0].Outcome != teamaudit.OutcomeOK {
		t.Fatalf("privileged add rows: %+v", rows)
	}
}

// The authorization table of every Group route with delegation on: who
// reads and who writes which Group.
func TestTeamDelegationTable(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	reads := []struct {
		as, path string
		want     int
		code     string
	}{
		{"webLeader", "/UYELER/ARGE/WEBLAB", fiber.StatusOK, ""},
		{"webCoord", "/UYELER/ARGE/WEBLAB", fiber.StatusOK, ""},
		{"webLeader", "/UYELER/ARGE/WEBLAB/FRONT", fiber.StatusOK, ""},
		{"webLeader", "/UYELER/ARGE/WEBLAB/LIDERLER", fiber.StatusOK, ""},
		{"frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", fiber.StatusOK, ""},
		{"frontLeader", "/UYELER/ARGE/WEBLAB", fiber.StatusForbidden, ""},
		{"mobLeader", "/UYELER/ARGE/WEBLAB", fiber.StatusForbidden, ""},
		{"mobLeader", "/UYELER/ARGE/WEBLAB/FRONT", fiber.StatusForbidden, ""},
		{"member", "/UYELER/ARGE/WEBLAB", fiber.StatusForbidden, ""},
		{"outsider", "/UYELER/ARGE/WEBLAB", fiber.StatusForbidden, ""},
		{"webLeader", "/UYELER/ARGE", fiber.StatusForbidden, ""},
		{"webLeader", "/UYELER", fiber.StatusForbidden, ""},
		{"webLeader", "/UYELER/YK", fiber.StatusForbidden, ""},
		{"webLeader", "/UYELER/ARGE/WEBLAB/SECURE", fiber.StatusForbidden, authz.RefusalProtectedGroup},
		{"mobLeader", "/UYELER/ARGE/WEBLAB/SECURE", fiber.StatusForbidden, ""},
		{"yk", "/UYELER/ARGE/WEBLAB/SECURE", fiber.StatusOK, ""},
		{"yk", "/UYELER/ARGE", fiber.StatusOK, ""},
	}
	for _, r := range reads {
		for _, suffix := range []string{"", "/members"} {
			got := e.send(e.token(r.as), fiber.MethodGet, e.group(r.path)+suffix, "")
			wantStatus(t, r.as+" GET "+r.path+suffix, got, r.want, r.code)
		}
	}
	if got := e.send("", fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB"), ""); got.status != fiber.StatusUnauthorized {
		t.Errorf("anonymous: %d", got.status)
	}
	// A Group named by its path or name, not its id: the Privileged tool's
	// shortcut only.
	for _, ref := range []string{"%2FUYELER%2FARGE%2FWEBLAB", "WEBLAB"} {
		wantStatus(t, "by "+ref, e.send(e.token("webLeader"), fiber.MethodGet, "/v1/groups/"+ref, ""), fiber.StatusForbidden, "")
	}
	// The whole list and a new top Group stay Privileged's (Forms reads the
	// list), as do renames, attributes and role mappings.
	web := e.ids["/UYELER/ARGE/WEBLAB"]
	for _, r := range []struct{ method, path, body string }{
		{fiber.MethodGet, "/v1/groups", ""},
		{fiber.MethodPost, "/v1/groups", `{"name":"NEWLAB","parentId":"` + web + `"}`},
		{fiber.MethodPatch, "/v1/groups/" + web, `{"attributes":{"public_listing":"true"}}`},
		{fiber.MethodGet, "/v1/groups/" + web + "/client-roles", ""},
		{fiber.MethodPut, "/v1/groups/" + web + "/client-roles", `[]`},
	} {
		wantStatus(t, "webLeader "+r.method+" "+r.path, e.send(e.token("webLeader"), r.method, r.path, r.body), fiber.StatusForbidden, "")
	}

	writes := []struct {
		name          string
		as, op, path  string
		who           string
		want          int
		code          string
		basis         teamaudit.Basis
		via, rowGroup string
	}{
		{"a Leader adds to their Team", "webLeader", "add", "/UYELER/ARGE/WEBLAB", "newcomer", fiber.StatusNoContent, "", teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB"},
		{"a Leader adds to a Sub-team", "webLeader", "add", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer", fiber.StatusNoContent, "", teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/FRONT"},
		{"a Sub-team's Leader adds to it", "frontLeader", "add", "/UYELER/ARGE/WEBLAB/FRONT", "outsider", fiber.StatusNoContent, "", teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/FRONT"},
		{"a Leader makes a Sub-team's Leader", "webLeader", "add", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER", "frontMember", fiber.StatusNoContent, "", teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER"},
		{"a Sub-team's Leader removes from it", "frontLeader", "remove", "/UYELER/ARGE/WEBLAB/FRONT", "outsider", fiber.StatusNoContent, "", teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/FRONT"},
		// frontMember is now in FRONT and its LIDERLER: the roster's Source
		// group is the nearer one, FRONT.
		{"a Leader removes a Sub-team's member", "webLeader", "remove", "/UYELER/ARGE/WEBLAB/FRONT", "frontMember", fiber.StatusNoContent, "", teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/FRONT"},
		// The bad Leader (SPEC §12).
		{"a Leader makes themselves Leader again", "webLeader", "add", "/UYELER/ARGE/WEBLAB/LIDERLER", "webLeader", fiber.StatusForbidden, authz.RefusalLeaderGroupNeedsParent, teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{"a Leader makes a co-Leader (S2)", "webLeader", "add", "/UYELER/ARGE/WEBLAB/LIDERLER", "member", fiber.StatusForbidden, authz.RefusalLeaderGroupNeedsParent, teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{"a Coordinator makes themselves a Leader", "webCoord", "add", "/UYELER/ARGE/WEBLAB/LIDERLER", "webCoord", fiber.StatusForbidden, authz.RefusalLeaderGroupNeedsParent, teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{"a Sub-team's Leader makes a co-Leader", "frontLeader", "add", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER", "member", fiber.StatusForbidden, authz.RefusalLeaderGroupNeedsParent, teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER"},
		{"a Leader leaves their own Leader group", "webLeader", "remove", "/UYELER/ARGE/WEBLAB", "webLeader", fiber.StatusForbidden, authz.RefusalLeaderGroupNeedsParent, teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{"a Leader removes their co-Coordinator", "webLeader", "remove", "/UYELER/ARGE/WEBLAB/KOORDINATORLER", "webCoord", fiber.StatusForbidden, authz.RefusalLeaderGroupNeedsParent, teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/KOORDINATORLER"},
		{"a Leader adds to a sibling Team", "webLeader", "add", "/UYELER/ARGE/MOBILAB", "newcomer", fiber.StatusForbidden, "", teamaudit.BasisNone, "", "/UYELER/ARGE/MOBILAB"},
		{"a Leader removes from a sibling Team", "mobLeader", "remove", "/UYELER/ARGE/WEBLAB", "member", fiber.StatusForbidden, "", teamaudit.BasisNone, "", "/UYELER/ARGE/WEBLAB"},
		{"a Sub-team's Leader adds to the Team above", "frontLeader", "add", "/UYELER/ARGE/WEBLAB", "newcomer", fiber.StatusForbidden, "", teamaudit.BasisNone, "", "/UYELER/ARGE/WEBLAB"},
		{"a Leader adds to a protected Group", "webLeader", "add", "/UYELER/ARGE/WEBLAB/SECURE", "newcomer", fiber.StatusForbidden, authz.RefusalProtectedGroup, teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/SECURE"},
		{"a Leader adds to an area", "webLeader", "add", "/UYELER/ARGE", "newcomer", fiber.StatusForbidden, "", teamaudit.BasisNone, "", "/UYELER/ARGE"},
		{"a Leader adds to YK", "webLeader", "add", "/UYELER/YK", "webLeader", fiber.StatusForbidden, "", teamaudit.BasisNone, "", "/UYELER/YK"},
		{"a member adds to their Team", "member", "add", "/UYELER/ARGE/WEBLAB", "newcomer", fiber.StatusForbidden, "", teamaudit.BasisNone, "", "/UYELER/ARGE/WEBLAB"},
		// Through the Sub-team's roster, from its Leader group.
		{"a Leader removes a Sub-team's Leader", "webLeader", "remove", "/UYELER/ARGE/WEBLAB/FRONT", "frontLeader", fiber.StatusNoContent, "", teamaudit.BasisLeader, "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER"},
	}
	for _, w := range writes {
		before := len(e.audit.Rows())
		var got rawResponse
		if w.op == "add" {
			got = e.add(w.as, w.path, w.who)
		} else {
			got = e.remove(w.as, w.path, w.who)
		}
		wantStatus(t, w.name, got, w.want, w.code)
		rows := e.audit.Rows()[before:]
		if len(rows) != 1 {
			t.Errorf("%s: %d rows, want 1", w.name, len(rows))
			continue
		}
		row := rows[0]
		wantOutcome := teamaudit.OutcomeOK
		if w.want != fiber.StatusNoContent {
			wantOutcome = teamaudit.OutcomeDenied
		}
		wantCode := w.code
		if wantOutcome == teamaudit.OutcomeDenied && wantCode == "" {
			wantCode = "forbidden"
		}
		if row.Outcome != wantOutcome || row.ErrorCode != wantCode || row.Actor.Basis != w.basis || row.Actor.ViaGroupID != e.ids[w.via] ||
			row.GroupID != e.ids[w.rowGroup] || row.GroupPath != w.rowGroup || row.TargetUserID != e.people[w.who] || row.Actor.ID != e.people[w.as] {
			t.Errorf("%s: row %+v", w.name, row)
		}
	}
	// The team membership mail hears of each change, the Leader as its
	// actor, and of no refused one.
	heard := e.heard.all()
	if len(heard) != 7 || heard[0].UserID != e.people["newcomer"] || !heard[0].Added || heard[0].ActorID != e.people["webLeader"].String() ||
		heard[0].Group.Path != "/UYELER/ARGE/WEBLAB" || heard[4].Added || heard[4].Group.Path != "/UYELER/ARGE/WEBLAB/FRONT" {
		t.Errorf("heard %+v", heard)
	}
	if !e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB") || !e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB/FRONT") ||
		e.directlyIn("newcomer", "/UYELER/ARGE/MOBILAB") || e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB/SECURE") ||
		!e.directlyIn("webLeader", "/UYELER/ARGE/WEBLAB/LIDERLER") || e.directlyIn("member", "/UYELER/ARGE/WEBLAB/LIDERLER") ||
		!e.directlyIn("webCoord", "/UYELER/ARGE/WEBLAB/KOORDINATORLER") || e.directlyIn("outsider", "/UYELER/ARGE/WEBLAB/FRONT") ||
		e.directlyIn("frontMember", "/UYELER/ARGE/WEBLAB/FRONT") || !e.directlyIn("frontMember", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER") ||
		e.directlyIn("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER") {
		for _, who := range []string{"newcomer", "webLeader", "member", "webCoord", "outsider", "frontMember"} {
			groups, _ := e.dir.GroupsForUser(context.Background(), e.people[who])
			t.Logf("%s: %v", who, identity.GroupPaths(groups))
		}
		t.Fatal("the memberships are not what the table allowed")
	}
}

// A Leader removed from their Leader group in Keycloak manages nothing,
// whatever their token still says: a write reads the current membership.
func TestTeamDelegationStaleToken(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	stale := e.token("exLeader", "/UYELER/ARGE/WEBLAB/LIDERLER")
	for _, r := range []struct{ method, path, body string }{
		{fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB") + "/members", `{"userId":"` + e.people["newcomer"].String() + `"}`},
		{fiber.MethodDelete, e.group("/UYELER/ARGE/WEBLAB") + "/members/" + e.people["member"].String(), ""},
		{fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB") + "/members", ""},
	} {
		wantStatus(t, "stale "+r.method, e.send(stale, r.method, r.path, r.body), fiber.StatusForbidden, "")
	}
	// Removed while their reads are remembered: a write still reads fresh.
	leader := e.token("webLeader")
	if got := e.send(leader, fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB"), ""); got.status != fiber.StatusOK {
		t.Fatalf("leader read: %d", got.status)
	}
	if err := e.dir.RemoveMember(context.Background(), e.ids["/UYELER/ARGE/WEBLAB/LIDERLER"], e.people["webLeader"]); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "removed leader writes", e.send(leader, fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB")+"/members", `{"userId":"`+e.people["newcomer"].String()+`"}`), fiber.StatusForbidden, "")
	if e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB") || !e.directlyIn("member", "/UYELER/ARGE/WEBLAB") {
		t.Fatal("a stale token changed a membership")
	}
}

// The limits of a delegated add: an active account only, at most the
// Group limit of direct Groups, the minute's budget of writes; and the
// tree checks: a Group whose path lies, a Group on the way that shares its
// name with another one.
func TestTeamDelegationLimitsAndTreeChecks(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, groupsCap: 3, writes: 100})
	busy := e.person("busy", "/UYELER/ARGE/MOBILAB")
	for _, path := range []string{"/UYELER/ARGE/WEBLAB/SECURE", "/UYELER/ORGANIZASYON"} {
		if err := e.dir.AddMember(context.Background(), e.ids[path], busy); err != nil {
			t.Fatal(err)
		}
	}
	wantStatus(t, "group limit", e.add("webLeader", "/UYELER/ARGE/WEBLAB", "busy"), fiber.StatusConflict, identity.CodeGroupLimit)
	// Already in it: no new Group, no limit.
	if err := e.dir.AddMember(context.Background(), e.ids["/UYELER/ARGE/WEBLAB/FRONT"], busy); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "repeat add at the limit", e.add("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "busy"), fiber.StatusNoContent, "")

	gone := e.person("gone", "")
	if _, err := e.users.RequestDeletion(context.Background(), gone, nil); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "deletion pending", e.add("webLeader", "/UYELER/ARGE/WEBLAB", "gone"), fiber.StatusNotFound, "")
	stranger := uuid.New()
	e.dir.PutUser(identity.Person{ID: stranger, Email: "s@example.test"})
	wantStatus(t, "no core account", e.send(e.token("webLeader"), fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB")+"/members", `{"userId":"`+stranger.String()+`"}`), fiber.StatusNotFound, "")

	// A name with a "/": Keycloak's path reads as a Group under FRONT.
	slash := identity.Group{ID: uuid.NewString(), Name: "FRONT/X", Path: "/UYELER/ARGE/WEBLAB/FRONT/X"}
	e.dir.PutGroup(slash)
	e.dir.SetGroupParent(slash.ID, e.ids["/UYELER/ARGE/WEBLAB"])
	before := len(e.audit.Rows())
	got := e.send(e.token("webLeader"), fiber.MethodPost, "/v1/groups/"+slash.ID+"/members", `{"userId":"`+e.people["newcomer"].String()+`"}`)
	wantStatus(t, "slash name", got, fiber.StatusConflict, identity.CodeGroupTreeMismatch)
	if rows := e.audit.Rows()[before:]; len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomeDenied || rows[0].ErrorCode != identity.CodeGroupTreeMismatch {
		t.Fatalf("slash name rows %+v", rows)
	}
	wantStatus(t, "slash name read", e.send(e.token("webLeader"), fiber.MethodGet, "/v1/groups/"+slash.ID, ""), fiber.StatusConflict, identity.CodeGroupTreeMismatch)

	// FRONT's code is held by a Group elsewhere too (an old duplicate).
	e.dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "front", Path: "/UYELER/ORGANIZASYON/front"})
	fresh := newDelegationEnvLike(e)
	wantStatus(t, "duplicate code", fresh.add("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer"), fiber.StatusConflict, identity.CodeTeamCodeNotUnique)
	wantStatus(t, "duplicate code below", fresh.add("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer"), fiber.StatusConflict, identity.CodeTeamCodeNotUnique)
	wantStatus(t, "no duplicate on the way", fresh.add("webLeader", "/UYELER/ARGE/WEBLAB", "newcomer"), fiber.StatusNoContent, "")
	if fresh.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB/FRONT") {
		t.Fatal("added under a duplicate code")
	}
}

// newDelegationEnvLike is an app over e's directory and users with a new
// TeamDelegation: nothing of e's remembered reads.
func newDelegationEnvLike(e *delegationEnv) *delegationEnv {
	out := *e
	policy := authz.DefaultPolicy()
	policy.TeamDelegation = true
	az := authz.NewAuthorizer(policy)
	delegation := identity.NewTeamDelegation(policy, e.dir, e.dir, identity.TeamDelegationOptions{})
	options := identity.Options{TeamCodes: teamcode.NewMemoryLedger(), TeamAudit: e.audit, Delegation: delegation}
	deps := memoryDepsDecidedBy(e.dir, options, az)
	deps.Identity = identity.NewServiceWithOptions(identity.AuditedDirectory(e.dir, e.audit, nil), e.users, az, options)
	deps.ParseToken = e.keys.Parse()
	deps.ManagedTeams = delegation
	out.app = httpx.New(deps)
	return &out
}

func TestTeamDelegationRateLimit(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, writes: 2})
	wantStatus(t, "first", e.add("webLeader", "/UYELER/ARGE/WEBLAB", "newcomer"), fiber.StatusNoContent, "")
	wantStatus(t, "second", e.add("webLeader", "/UYELER/ARGE/MOBILAB", "newcomer"), fiber.StatusForbidden, "")
	got := e.add("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer")
	wantStatus(t, "third", got, fiber.StatusTooManyRequests, identity.CodeRateLimited)
	if e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB/FRONT") {
		t.Fatal("a write past the budget went through")
	}
	// Another Leader has their own budget; Privileged has none.
	wantStatus(t, "another leader", e.add("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer"), fiber.StatusNoContent, "")
	for range 3 {
		wantStatus(t, "privileged", e.add("yk", "/UYELER/ARGE/MOBILAB", "newcomer"), fiber.StatusNoContent, "")
	}
}

// No audit row, no change: a delegated write whose row cannot be opened is
// 503 and Keycloak is not asked.
func TestTeamDelegationAuditUnavailable(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	e.audit.SetUnavailable(true)
	before := len(e.dir.Ops)
	wantStatus(t, "audit down", e.add("webLeader", "/UYELER/ARGE/WEBLAB", "newcomer"), fiber.StatusServiceUnavailable, "audit_unavailable")
	if slices.Contains(e.dir.Ops[before:], "AddMember") || e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB") {
		t.Fatal("added without an audit row")
	}
	// A refusal stays a refusal: its row is lost, logged, and nothing changes.
	wantStatus(t, "audit down, refused", e.add("webLeader", "/UYELER/ARGE/MOBILAB", "newcomer"), fiber.StatusForbidden, "")
}

// The roster a Leader reads carries only what SPEC §6.4 lets them see.
func TestTeamDelegationRosterFields(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	got := e.send(e.token("webLeader"), fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB")+"/members", "")
	if got.status != fiber.StatusOK {
		t.Fatalf("%d %s", got.status, got.body)
	}
	var members []map[string]any
	if err := json.Unmarshal(got.body, &members); err != nil {
		t.Fatal(err)
	}
	allowed := []string{"id", "email", "firstName", "lastName", "profilePictureUrl", "profilePictureSizes", "sourceGroupId", "sourceGroupPath",
		// The read status and the name the person reads as (activePerson).
		"status", "displayName"}
	seenSource := false
	for _, m := range members {
		for key := range m {
			if !slices.Contains(allowed, key) {
				t.Errorf("a Leader sees %s: %v", key, m)
			}
		}
		if m["sourceGroupPath"] == "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER" {
			seenSource = true
		}
	}
	if len(members) < 5 || !seenSource {
		t.Fatalf("roster %s", got.body)
	}
	// Privileged's roster is as before: the directory's fields.
	if got := e.send(e.token("yk"), fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB")+"/members", ""); !strings.Contains(string(got.body), `"username"`) {
		t.Fatalf("privileged roster %s", got.body)
	}
}

func TestTeamDelegationMemberCandidates(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	search := func(as, path, q string) rawResponse {
		return e.send(e.token(as), fiber.MethodGet, e.group(path)+"/member-candidates?q="+q, "")
	}
	decode := func(got rawResponse) []identity.MemberCandidate {
		t.Helper()
		var out []identity.MemberCandidate
		if err := json.Unmarshal(got.body, &out); err != nil || got.status != fiber.StatusOK {
			t.Fatalf("%d %s %v", got.status, got.body, err)
		}
		return out
	}
	wantStatus(t, "short query", search("webLeader", "/UYELER/ARGE/WEBLAB", "ne"), fiber.StatusBadRequest, "")
	wantStatus(t, "a sibling's Leader", search("mobLeader", "/UYELER/ARGE/WEBLAB", "newcomer"), fiber.StatusForbidden, "")
	wantStatus(t, "a member", search("member", "/UYELER/ARGE/WEBLAB", "newcomer"), fiber.StatusForbidden, "")

	found := decode(search("webLeader", "/UYELER/ARGE/WEBLAB", "newco"))
	if len(found) != 1 || found[0].ID != e.people["newcomer"] || found[0].Email != "n***@example.test" || found[0].AlreadyMember {
		t.Fatalf("masked %+v", found)
	}
	found = decode(search("webLeader", "/UYELER/ARGE/WEBLAB", "member@example.test"))
	if len(found) != 1 || found[0].Email != "member@example.test" || !found[0].AlreadyMember {
		t.Fatalf("exact %+v", found)
	}
	// The fields a Leader may not probe: the Sky number, the username.
	if found := decode(search("webLeader", "/UYELER/ARGE/WEBLAB", "SKY-newcomer")); len(found) != 0 {
		t.Fatalf("by Sky number %+v", found)
	}
	if found := decode(search("webLeader", "/UYELER/ARGE/WEBLAB", "u-newcomer")); len(found) != 0 {
		t.Fatalf("by username %+v", found)
	}
	for i := range 12 {
		e.person("aday"+string(rune('a'+i)), "")
	}
	if found := decode(search("webLeader", "/UYELER/ARGE/WEBLAB", "aday")); len(found) != 10 {
		t.Fatalf("limit: %d", len(found))
	}
	gone := e.person("adaygone", "")
	if _, err := e.users.RequestDeletion(context.Background(), gone, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range decode(search("webLeader", "/UYELER/ARGE/WEBLAB", "adaygone")) {
		t.Fatalf("an account being erased: %+v", c)
	}
	if found := decode(search("yk", "/UYELER/ARGE/MOBILAB", "newcomer")); len(found) != 1 {
		t.Fatalf("privileged %+v", found)
	}
}

func TestTeamDelegationManagedGroupsAndCapabilities(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	tree := func(as string) []identity.ManagedNode {
		t.Helper()
		got := e.send(e.token(as), fiber.MethodGet, "/v1/users/me/managed-groups", "")
		var out []identity.ManagedNode
		if err := json.Unmarshal(got.body, &out); err != nil || got.status != fiber.StatusOK {
			t.Fatalf("%s: %d %s", as, got.status, got.body)
		}
		return out
	}
	paths := func(nodes []identity.ManagedNode) []string {
		var out []string
		var walk func([]identity.ManagedNode, string)
		walk = func(nodes []identity.ManagedNode, indent string) {
			for _, n := range nodes {
				out = append(out, indent+n.Path)
				walk(n.Children, indent+" ")
			}
		}
		walk(nodes, "")
		return out
	}
	web := tree("webLeader")
	if got := paths(web); !slices.Equal(got, []string{"/UYELER/ARGE/WEBLAB", " /UYELER/ARGE/WEBLAB/FRONT", " /UYELER/ARGE/WEBLAB/SECURE"}) {
		t.Fatalf("webLeader tree %v", got)
	}
	if n := web[0]; !n.Can["group.read"] || !n.Can["members.write"] || n.Can["leaders.write"] || n.LeaderCount != 2 || n.MemberCount != 1 || n.Code != "WEBLAB" {
		t.Fatalf("WEBLAB node %+v", n)
	}
	if front := web[0].Children[0]; !front.Can["leaders.write"] || front.LeaderCount != 1 {
		t.Fatalf("FRONT node %+v", front)
	}
	if secure := web[0].Children[1]; !secure.Protected || secure.Can["group.read"] || secure.Can["members.write"] {
		t.Fatalf("SECURE node %+v", secure)
	}
	// Leading both WEBLAB and FRONT: FRONT once, under WEBLAB.
	if err := e.dir.AddMember(context.Background(), e.ids["/UYELER/ARGE/WEBLAB/FRONT/LIDERLER"], e.people["webCoord"]); err != nil {
		t.Fatal(err)
	}
	if got := paths(tree("webCoord")); len(got) != 3 || got[0] != "/UYELER/ARGE/WEBLAB" {
		t.Fatalf("webCoord tree %v", got)
	}
	if got := paths(tree("frontLeader")); !slices.Equal(got, []string{"/UYELER/ARGE/WEBLAB/FRONT"}) {
		t.Fatalf("frontLeader tree %v", got)
	}
	if got := tree("member"); len(got) != 0 {
		t.Fatalf("member tree %v", got)
	}
	if got := paths(tree("yk")); !slices.Equal(got, []string{"/UYELER/ARGE/MOBILAB", "/UYELER/ARGE/WEBLAB", " /UYELER/ARGE/WEBLAB/FRONT", " /UYELER/ARGE/WEBLAB/SECURE"}) {
		t.Fatalf("yk tree %v", got)
	}

	capabilities := func(as string) (teams []identity.ManagedTeam, can map[string]bool) {
		t.Helper()
		got := e.send(e.token(as), fiber.MethodGet, "/v1/users/me/capabilities", "")
		var answer struct {
			ManagedTeams []identity.ManagedTeam `json:"managedTeams"`
			Can          map[string]bool        `json:"can"`
		}
		if err := json.Unmarshal(got.body, &answer); err != nil || got.status != fiber.StatusOK || answer.ManagedTeams == nil {
			t.Fatalf("%s: %d %s", as, got.status, got.body)
		}
		return answer.ManagedTeams, answer.Can
	}
	teams, can := capabilities("webLeader")
	if len(teams) != 1 || teams[0].ID != e.ids["/UYELER/ARGE/WEBLAB"] || teams[0].Code != "WEBLAB" || teams[0].DisplayName.TR != "WEBLAB" || can["audit.read"] {
		t.Fatalf("webLeader: %+v %v", teams, can["audit.read"])
	}
	if teams, _ := capabilities("webCoord"); len(teams) != 2 {
		t.Fatalf("webCoord: %+v", teams)
	}
	for _, as := range []string{"member", "yk", "outsider"} {
		if teams, _ := capabilities(as); len(teams) != 0 {
			t.Fatalf("%s: %+v", as, teams)
		}
	}
}

// A realm role, or a composite client role that holds a protected role or
// a realm role, makes the Group and everything under it protected (review
// of #235, B1): a Leader can no longer hand out realm-management or core's
// permission roles through a mapping Privileged made. A composite of
// harmless roles (cms:access) protects nothing.
func TestTeamDelegationRealmAndCompositeRolesProtect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		on      string
		roles   []identity.ClientRole
		setup   func(*identity.Memory)
		target  string
		refused bool
	}{
		{name: "a realm role on the Sub-team", on: "/UYELER/ARGE/WEBLAB/FRONT",
			roles: []identity.ClientRole{{Role: "offline_access"}}, target: "/UYELER/ARGE/WEBLAB/FRONT", refused: true},
		{name: "a realm role on the area", on: "/UYELER/ARGE",
			roles: []identity.ClientRole{{Role: "some-realm-role"}}, target: "/UYELER/ARGE/WEBLAB", refused: true},
		{name: "a composite holding realm-management on the area", on: "/UYELER/ARGE",
			roles: []identity.ClientRole{{ClientID: "skymail", Role: "staff"}},
			setup: func(m *identity.Memory) {
				m.PutCompositeRole(identity.ClientRole{ClientID: "skymail", Role: "staff"},
					identity.ClientRole{ClientID: "realm-management", Role: "manage-users"}, identity.ClientRole{ClientID: "core", Role: authz.RoleGroupsManage})
			}, target: "/UYELER/ARGE/WEBLAB", refused: true},
		{name: "a composite two deep holding groups:manage", on: "/UYELER/ARGE/WEBLAB",
			roles: []identity.ClientRole{{ClientID: "skymail", Role: "outer"}},
			setup: func(m *identity.Memory) {
				m.PutCompositeRole(identity.ClientRole{ClientID: "skymail", Role: "outer"}, identity.ClientRole{ClientID: "forms", Role: "inner"})
				m.PutCompositeRole(identity.ClientRole{ClientID: "forms", Role: "inner"}, identity.ClientRole{ClientID: "core", Role: authz.RoleGroupsManage})
			}, target: "/UYELER/ARGE/WEBLAB/FRONT", refused: true},
		{name: "a composite holding a realm role", on: "/UYELER/ARGE/WEBLAB",
			roles: []identity.ClientRole{{ClientID: "skymail", Role: "outer"}},
			setup: func(m *identity.Memory) {
				m.PutCompositeRole(identity.ClientRole{ClientID: "skymail", Role: "outer"}, identity.ClientRole{Role: "admin"})
			}, target: "/UYELER/ARGE/WEBLAB", refused: true},
		{name: "cms:access and its content roles", on: "/UYELER/ARGE/WEBLAB",
			roles: []identity.ClientRole{{ClientID: "frontend-main", Role: "cms:access"}},
			setup: func(m *identity.Memory) {
				m.PutCompositeRole(identity.ClientRole{ClientID: "frontend-main", Role: "cms:access"},
					identity.ClientRole{ClientID: "frontend-main", Role: "content:read"}, identity.ClientRole{ClientID: "frontend-main", Role: "content:write"})
			}, target: "/UYELER/ARGE/WEBLAB/FRONT", refused: false},
	} {
		e := newDelegationEnv(t, delegationSetup{on: true})
		if tc.setup != nil {
			tc.setup(e.dir)
		}
		if err := e.dir.SetGroupClientRoles(context.Background(), e.ids[tc.on], tc.roles); err != nil {
			t.Fatal(err)
		}
		got := e.add("webLeader", tc.target, "newcomer")
		if tc.refused {
			wantStatus(t, tc.name, got, fiber.StatusForbidden, authz.RefusalProtectedGroup)
			if e.directlyIn("newcomer", tc.target) {
				t.Errorf("%s: added", tc.name)
			}
			continue
		}
		wantStatus(t, tc.name, got, fiber.StatusNoContent, "")
	}
}

// A write reads the Groups' protection fresh: a role mapped in Keycloak
// (its console, a script, another core) after a Leader's read is seen at
// once (review of #235, O1).
func TestTeamDelegationWriteReadsProtectionFresh(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	if got := e.send(e.token("webLeader"), fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB/FRONT")+"/members", ""); got.status != fiber.StatusOK {
		t.Fatalf("read: %d", got.status)
	}
	if err := e.dir.SetGroupClientRoles(context.Background(), e.ids["/UYELER/ARGE/WEBLAB/FRONT"], []identity.ClientRole{{ClientID: "core", Role: authz.RoleUsersManage}}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "after the mapping", e.add("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer"), fiber.StatusForbidden, authz.RefusalProtectedGroup)
}

// A Group named like a leader subgroup but not one (LIDERLER-ESKI, a
// trailing blank) takes no delegated member (review of #235, D1).
func TestTeamDelegationLeaderLookAlikes(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	for _, name := range []string{"LIDERLER-ESKI", "LIDERLER "} {
		g := identity.Group{ID: uuid.NewString(), Name: name, Path: "/UYELER/ARGE/WEBLAB/" + name}
		e.dir.PutGroup(g)
		got := e.send(e.token("webLeader"), fiber.MethodPost, "/v1/groups/"+g.ID+"/members", `{"userId":"`+e.people["newcomer"].String()+`"}`)
		wantStatus(t, name, got, fiber.StatusForbidden, "")
	}
	if e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB/LIDERLER-ESKI") {
		t.Fatal("added to a look-alike")
	}
}

// A Leader whose own account is being erased writes nothing (review of
// #235, D3).
func TestTeamDelegationInactiveCaller(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	token := e.token("webLeader")
	if _, err := e.users.RequestDeletion(context.Background(), e.people["webLeader"], nil); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "being erased", e.send(token, fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB")+"/members", `{"userId":"`+e.people["newcomer"].String()+`"}`), fiber.StatusForbidden, "")
	if e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB") {
		t.Fatal("added by an inactive caller")
	}
}

// The candidate search matches an address only whole: a part of one is no
// oracle for the masked address (review of #235, O2); and a Leader's
// searches have a budget.
func TestTeamDelegationCandidateMaskAndBudget(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, searches: 3})
	id := uuid.New()
	e.dir.PutUser(identity.Person{ID: id, Email: "zeynep.k1907@gmail.test", FirstName: "Ayşe", LastName: "Yılmaz"})
	if _, _, err := e.users.Upsert(context.Background(), user.User{ID: id, Email: "zeynep.k1907@gmail.test", FirstName: "Ayşe", LastName: "Yılmaz"}); err != nil {
		t.Fatal(err)
	}
	search := func(q string) []identity.MemberCandidate {
		t.Helper()
		got := e.send(e.token("webLeader"), fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB")+"/member-candidates?q="+q, "")
		var out []identity.MemberCandidate
		if err := json.Unmarshal(got.body, &out); err != nil || got.status != fiber.StatusOK {
			t.Fatalf("%s: %d %s", q, got.status, got.body)
		}
		return out
	}
	for _, q := range []string{"zey", "k1907"} {
		if found := search(q); len(found) != 0 {
			t.Errorf("%q matched the address: %+v", q, found)
		}
	}
	if found := search("Yılm"); len(found) != 1 || found[0].Email != "z***@gmail.test" {
		t.Errorf("by name: %+v", found)
	}
	wantStatus(t, "over the budget", e.send(e.token("webLeader"), fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB")+"/member-candidates?q=zeynep.k1907%40gmail.test", ""), fiber.StatusTooManyRequests, identity.CodeRateLimited)
	// Privileged has no budget.
	for range 4 {
		if got := e.send(e.token("yk"), fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB")+"/member-candidates?q=zeynep.k1907%40gmail.test", ""); got.status != fiber.StatusOK || !strings.Contains(string(got.body), "zeynep.k1907@gmail.test") {
			t.Fatalf("privileged exact: %d %s", got.status, got.body)
		}
	}
}

// One delegated write per person at a time (review of #235, B2): a second
// one while the first runs is 429, and the first goes through.
func TestTeamDelegationOneWriteAtATime(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	policy := authz.DefaultPolicy()
	policy.TeamDelegation = true
	az := authz.NewAuthorizer(policy)
	delegation := identity.NewTeamDelegation(policy, e.dir, e.dir, identity.TeamDelegationOptions{})
	options := identity.Options{TeamCodes: teamcode.NewMemoryLedger(), TeamAudit: e.audit, Delegation: delegation}
	deps := memoryDepsDecidedBy(e.dir, options, az)
	deps.Identity = identity.NewServiceWithOptions(identity.AuditedDirectory(slowAdds{e.dir, 400 * time.Millisecond}, e.audit, nil), e.users, az, options)
	deps.ParseToken = e.keys.Parse()
	e.app = httpx.New(deps)
	token := e.token("webLeader")
	statuses := make(chan int, 2)
	for _, who := range []string{"newcomer", "outsider"} {
		go func() {
			statuses <- e.send(token, fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB")+"/members", `{"userId":"`+e.people[who].String()+`"}`).status
		}()
		time.Sleep(100 * time.Millisecond)
	}
	got := []int{<-statuses, <-statuses}
	slices.Sort(got)
	// A write running is no budget spent: try again in a second.
	if retry := e.retryAfter(token); retry != "" && retry != "1" {
		t.Errorf("Retry-After %q", retry)
	}
	if got[0] != fiber.StatusNoContent || got[1] != fiber.StatusTooManyRequests {
		t.Fatalf("statuses %v", got)
	}
}

// countingLedger is a Team code ledger that counts its lock takings and
// may run something just before each, as if another request changed
// Keycloak between a write's first decision and its lock.
type countingLedger struct {
	teamcode.Ledger
	mu     sync.Mutex
	taken  int
	before func()
}

func (l *countingLedger) Locked(ctx context.Context, write func(context.Context, teamcode.Records) error) error {
	l.mu.Lock()
	l.taken++
	before := l.before
	l.mu.Unlock()
	if before != nil {
		before()
	}
	return l.Ledger.Locked(ctx, write)
}

// failingProtection is the memory directory whose protection reads fail
// for every Group but the Privileged root the canary reads.
type failingProtection struct {
	*identity.Memory
	root string
}

func (f failingProtection) GroupProtection(ctx context.Context, id string) (identity.Protection, error) {
	if id == f.root {
		return f.Memory.GroupProtection(ctx, id)
	}
	return identity.Protection{}, errors.New("keycloak: role mappings unavailable")
}

// appWith is e's app with the delegation reading reads and the service
// locking with ledger.
func (e *delegationEnv) appWith(reads identity.TeamReads, ledger teamcode.Ledger) {
	policy := authz.DefaultPolicy()
	policy.TeamDelegation = true
	az := authz.NewAuthorizer(policy)
	delegation := identity.NewTeamDelegation(policy, e.dir, reads, identity.TeamDelegationOptions{})
	options := identity.Options{TeamCodes: ledger, TeamAudit: e.audit, Delegation: delegation}
	deps := memoryDepsDecidedBy(e.dir, options, az)
	deps.Identity = identity.NewServiceWithOptions(identity.AuditedDirectory(e.dir, e.audit, nil), e.users, az, options)
	deps.ParseToken = e.keys.Parse()
	e.app = httpx.New(deps)
}

// The decision under the lock is made again on fresh reads: a Leader who
// loses their Leader group between the first decision and the lock writes
// nothing (review of #235, delta M1).
func TestTeamDelegationDecidesAgainUnderTheLock(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	ledger := &countingLedger{Ledger: teamcode.NewMemoryLedger()}
	ledger.before = func() {
		_ = e.dir.RemoveMember(context.Background(), e.ids["/UYELER/ARGE/WEBLAB/LIDERLER"], e.people["webLeader"])
	}
	e.appWith(e.dir, ledger)
	got := e.send(e.token("webLeader", "/UYELER/ARGE/WEBLAB/LIDERLER"), fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB")+"/members", `{"userId":"`+e.people["newcomer"].String()+`"}`)
	wantStatus(t, "leader removed before the lock", got, fiber.StatusForbidden, "")
	if ledger.taken != 1 || e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB") {
		t.Fatalf("lock taken %d times, added %v", ledger.taken, e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB"))
	}
}

// A caller the first decision refuses never takes the lock (review of
// #235, B2 and delta M6); one it allows takes it once.
func TestTeamDelegationRefusesBeforeTheLock(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	ledger := &countingLedger{Ledger: teamcode.NewMemoryLedger()}
	e.appWith(e.dir, ledger)
	for _, tc := range []struct{ as, path string }{
		{"outsider", "/UYELER/ARGE/WEBLAB"},
		{"member", "/UYELER/ARGE/WEBLAB"},
		{"mobLeader", "/UYELER/ARGE/WEBLAB"},
		{"webLeader", "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{"webLeader", "/UYELER/ARGE/WEBLAB/SECURE"},
	} {
		if got := e.add(tc.as, tc.path, "newcomer"); got.status != fiber.StatusForbidden {
			t.Errorf("%s %s: %d", tc.as, tc.path, got.status)
		}
	}
	if ledger.taken != 0 {
		t.Fatalf("refused writes took the lock %d times", ledger.taken)
	}
	wantStatus(t, "allowed", e.add("webLeader", "/UYELER/ARGE/WEBLAB", "newcomer"), fiber.StatusNoContent, "")
	if ledger.taken != 1 {
		t.Fatalf("an allowed write took the lock %d times", ledger.taken)
	}
}

// A protection that cannot be read refuses the write: never "not
// protected" (review of #235, delta M4).
func TestTeamDelegationProtectionReadFailsClosed(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	e.appWith(failingProtection{e.dir, e.ids["/UYELER/YK"]}, teamcode.NewMemoryLedger())
	got := e.add("webLeader", "/UYELER/ARGE/WEBLAB", "newcomer")
	if got.status < 500 || e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB") {
		t.Fatalf("status %d (%s), added %v", got.status, got.body, e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB"))
	}
}

// The canary (review of #235, O3): when no Privileged root reads as
// protected (a service account without view-clients reads every mapping
// as none), every delegated write is 503 and logged; reads go on.
func TestTeamDelegationCanary(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(e *delegationEnv){
		"the root's mappings read as none": func(e *delegationEnv) {
			_ = e.dir.SetGroupClientRoles(context.Background(), e.ids["/UYELER/YK"], nil)
		},
		"no root in the realm": func(e *delegationEnv) {
			e.dir.PutGroup(identity.Group{ID: e.ids["/UYELER/YK"], Name: "YK-OLD", Path: "/UYELER/YK-OLD"})
		},
	} {
		e := newDelegationEnv(t, delegationSetup{on: true})
		change(e)
		got := e.add("webLeader", "/UYELER/ARGE/WEBLAB", "newcomer")
		wantStatus(t, name, got, fiber.StatusServiceUnavailable, identity.CodeProtectionUnverified)
		if e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB") {
			t.Fatalf("%s: added", name)
		}
		if got := e.send(e.token("webLeader"), fiber.MethodGet, e.group("/UYELER/ARGE/WEBLAB")+"/members", ""); got.status != fiber.StatusOK {
			t.Fatalf("%s: read %d", name, got.status)
		}
		// Privileged's writes do not depend on it.
		yk := e.token("yk", "/UYELER/YK")
		wantStatus(t, name+", privileged", e.send(yk, fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB")+"/members", `{"userId":"`+e.people["newcomer"].String()+`"}`), fiber.StatusNoContent, "")
	}
}

// retryAfter is the Retry-After of a write sent while another of the same
// person runs, "" when none ran.
func (e *delegationEnv) retryAfter(token string) string {
	e.t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.send(token, fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB/FRONT")+"/members", `{"userId":"`+e.people["newcomer"].String()+`"}`)
	}()
	time.Sleep(100 * time.Millisecond)
	req := httptest.NewRequest(fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB/FRONT")+"/members", strings.NewReader(`{"userId":"`+e.people["outsider"].String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		e.t.Fatal(err)
	}
	_ = resp.Body.Close()
	<-done
	if resp.StatusCode != fiber.StatusTooManyRequests {
		e.t.Fatalf("second write: %d", resp.StatusCode)
	}
	return resp.Header.Get(fiber.HeaderRetryAfter)
}
