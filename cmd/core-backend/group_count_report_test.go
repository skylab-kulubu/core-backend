package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fakeRealm is a Keycloak with a few users and their direct Groups. It
// serves the user list and each user's groups the way Keycloak does, paged
// by first and max; the groups of failing fail with an error body that names
// the person.
type fakeRealm struct {
	users    []uuid.UUID
	disabled map[uuid.UUID]bool
	groups   map[uuid.UUID][]string
	failing  uuid.UUID
}

func (f *fakeRealm) serve(t *testing.T) *httptest.Server {
	t.Helper()
	const users = "/admin/realms/e-skylab/users"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/realms/e-skylab/protocol/openid-connect/token" {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("the report wrote to Keycloak: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == users {
			rows := make([]any, 0, len(f.users))
			for _, id := range f.users {
				rows = append(rows, map[string]any{
					"id": id.String(), "username": "u-" + id.String()[:8], "email": id.String()[:8] + "@example.com",
					"firstName": "Ada", "lastName": "Lovelace", "enabled": !f.disabled[id],
				})
			}
			_ = json.NewEncoder(w).Encode(fakePage(r, rows))
			return
		}
		raw, underUsers := strings.CutPrefix(r.URL.Path, users+"/")
		raw, groupsOfOne := strings.CutSuffix(raw, "/groups")
		id, err := uuid.Parse(raw)
		if !underUsers || !groupsOfOne || err != nil {
			t.Errorf("unexpected Keycloak call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if id == f.failing {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"` + id.String() + ` ada@example.com"}`))
			return
		}
		paths, known := f.groups[id]
		if !known {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"User not found"}`))
			return
		}
		rows := make([]any, 0, len(paths))
		for i, path := range paths {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("%s-%d", id, i), "name": path[strings.LastIndex(path, "/")+1:], "path": path})
		}
		_ = json.NewEncoder(w).Encode(fakePage(r, rows))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakePage(r *http.Request, rows []any) []any {
	first, err := strconv.Atoi(r.URL.Query().Get("first"))
	if err != nil {
		first = 0
	}
	rows = rows[min(first, len(rows)):]
	if limit, err := strconv.Atoi(r.URL.Query().Get("max")); err == nil && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows
}

func (f *fakeRealm) env(t *testing.T) func(string) string {
	srv := f.serve(t)
	return func(name string) string {
		return map[string]string{
			"KEYCLOAK_URL": srv.URL, "KEYCLOAK_REALM": "e-skylab",
			"KEYCLOAK_CLIENT_ID": "core", "KEYCLOAK_CLIENT_SECRET": "secret",
		}[name]
	}
}

// Users with none, one, two, 30 (the threshold) and 31 Group paths, and a
// disabled user with 40.
func groupCountRealm() (*fakeRealm, []uuid.UUID) {
	none, one, two, thirty, many, disabled := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	paths := func(prefix string, count int) []string {
		out := make([]string, 0, count)
		for i := 1; i <= count; i++ {
			out = append(out, fmt.Sprintf("/UYELER/%s%02d", prefix, i))
		}
		return out
	}
	ids := []uuid.UUID{none, one, two, thirty, many, disabled}
	return &fakeRealm{
		users:    ids,
		disabled: map[uuid.UUID]bool{disabled: true},
		groups: map[uuid.UUID][]string{
			none:     {},
			one:      {"/ADMIN"},
			two:      {"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/YK"},
			thirty:   paths("U", 30),
			many:     paths("T", 31),
			disabled: paths("D", 40),
		},
	}, ids
}

// The report counts each enabled user's direct Group paths and prints how
// they are spread, the most any user has, how many are above the Group
// overage threshold, and what the largest groups claim weighs in a token.
// A disabled user gets no token and is left out. It names nobody.
func TestGroupCountReportPrintsTheDistributionAndNamesNobody(t *testing.T) {
	realm, ids := groupCountRealm()
	var out, errOut bytes.Buffer

	code := runGroupCountReport(nil, realm.env(t), &out, &errOut)

	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out.String(), errOut.String())
	}
	for _, line := range []string{
		"users: 5",
		"disabled users (left out: they get no token): 1",
		"Group paths per user (paths: users):",
		"  0: 1",
		"  1: 1",
		"  2: 1",
		"  30: 1",
		"  31: 1",
		"most Group paths: 31",
		"users above 30 paths (Group overage): 1",
		// `"groups":[` (10) + 31 × `"/UYELER/Tnn"` (13) + 30 commas + `]`.
		"largest groups claim: 444 bytes of JSON, about 592 bytes in a token (base64url)",
		// 6 + 38 + 30 × 11 + 31 × 11 = 715 bytes over 64 paths (11.17);
		// `"groups":[]` (11) + 30 × (11.17 + 2 quotes) + 29 commas = 435.2,
		// × 4/3 = 580.2.
		"average Group path: 11.2 bytes; 30 of them: about 435 bytes of JSON, about 580 bytes in a token",
	} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Fatalf("report lacks %q:\n%s", line, out.String())
		}
	}
	all := out.String() + errOut.String()
	for _, id := range ids {
		if strings.Contains(all, id.String()) || strings.Contains(all, id.String()[:8]) {
			t.Fatalf("the report names a user:\n%s", all)
		}
	}
	for _, personal := range []string{"@example.com", "Ada", "Lovelace", "/UYELER", "/ADMIN"} {
		if strings.Contains(all, personal) {
			t.Fatalf("the report prints %q:\n%s", personal, all)
		}
	}
}

// -list-overage adds the users above the threshold, by sub (their Keycloak
// id), and nobody else.
func TestGroupCountReportListsOnlyTheUsersInGroupOverageBySub(t *testing.T) {
	realm, ids := groupCountRealm()
	var out, errOut bytes.Buffer

	code := runGroupCountReport([]string{"-list-overage"}, realm.env(t), &out, &errOut)

	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out.String(), errOut.String())
	}
	many := ids[4]
	if want := "users above 30 paths, by sub (sub: paths):\n  " + many.String() + ": 31\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("report lacks %q:\n%s", want, out.String())
	}
	for _, id := range append(ids[:4:4], ids[5]) {
		if strings.Contains(out.String(), id.String()) {
			t.Fatalf("the list names a user at or under the threshold, or disabled:\n%s", out.String())
		}
	}
	if strings.Contains(out.String()+errOut.String(), "@example.com") {
		t.Fatalf("the list prints an e-mail:\n%s", out.String())
	}
}

// A user whose Groups cannot be read is counted as unread, never as a user
// without Groups; the report still prints the rest and says it is
// incomplete, and the command fails. The error names nobody.
func TestGroupCountReportCountsUnreadUsersAndFails(t *testing.T) {
	realm, ids := groupCountRealm()
	realm.failing = ids[1]
	var out, errOut bytes.Buffer

	code := runGroupCountReport([]string{"-list-overage"}, realm.env(t), &out, &errOut)

	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s\n%s", code, out.String(), errOut.String())
	}
	for _, line := range []string{"users: 5", "  0: 1", "  2: 1", "  31: 1", "users whose Groups could not be read: 1 (the report is incomplete)"} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Fatalf("report lacks %q:\n%s", line, out.String())
		}
	}
	if strings.Contains(out.String(), "  1: 1\n") {
		t.Fatalf("the unread user is counted:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "status 500") {
		t.Fatalf("errors lack the Keycloak status:\n%s", errOut.String())
	}
	if all := out.String() + errOut.String(); strings.Contains(all, ids[1].String()) || strings.Contains(all, "@example.com") {
		t.Fatalf("the report names the unread user:\n%s", all)
	}
}

func TestGroupCountReportNeedsCoresKeycloakServiceAccount(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runGroupCountReport(nil, func(name string) string {
		if name == "KEYCLOAK_CLIENT_SECRET" {
			return "secret"
		}
		return ""
	}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "group-count-report needs KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID\n") || strings.Contains(errOut.String(), "secret") {
		t.Fatalf("exit %d:\n%s", code, errOut.String())
	}
	if code := runGroupCountReport([]string{"extra"}, func(string) string { return "set" }, &out, &errOut); code != 2 {
		t.Fatalf("an argument: exit %d", code)
	}
}
