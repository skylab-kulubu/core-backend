package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// GET /v1/team-audit-records (docs/team-audit.md, ekip-yetki-devri ticket
// 10): the audit:read role reads the record, nobody else.

const auditRecordsPath = "/v1/team-audit-records"

// newAuditReadEnv is the delegation realm, delegation on, with ADMIN and DK
// roots and a member of each.
func newAuditReadEnv(t *testing.T, mode authz.RoleMode) *delegationEnv {
	t.Helper()
	e := newDelegationEnv(t, delegationSetup{on: true, reads: true, mode: mode})
	for _, g := range []identity.Group{{Name: "ADMIN", Path: "/ADMIN"}, {Name: "DK", Path: "/UYELER/DK"}} {
		g.ID = uuid.NewString()
		e.ids[g.Path] = g.ID
		e.dir.PutGroup(g)
	}
	e.person("admin", "/ADMIN")
	e.person("dk", "/UYELER/DK")
	return e
}

// auditToken is name's token with their Groups and core's roles.
func (e *delegationEnv) auditToken(name string, coreRoles ...string) string {
	e.t.Helper()
	mine, err := e.dir.GroupsForUser(context.Background(), e.people[name])
	if err != nil {
		e.t.Fatal(err)
	}
	claims := jwt.MapClaims{
		"sub": e.people[name].String(), "email": name + "@example.test", "azp": "admin",
		"given_name": name, "family_name": "K", "groups": identity.GroupPaths(mine),
	}
	if len(coreRoles) > 0 {
		claims["resource_access"] = map[string]any{"core": map[string]any{"roles": coreRoles}}
	}
	return e.keys.Token(e.t, claims)
}

type auditAnswer struct {
	Items []struct {
		ID         uuid.UUID `json:"id"`
		OccurredAt time.Time `json:"occurredAt"`
		Action     string    `json:"action"`
		Actor      struct {
			ID          uuid.UUID `json:"id"`
			DisplayName string    `json:"displayName"`
			Basis       string    `json:"basis"`
			ViaGroupID  string    `json:"viaGroupId"`
			Client      string    `json:"client"`
		} `json:"actor"`
		Target *struct {
			ID          uuid.UUID `json:"id"`
			DisplayName string    `json:"displayName"`
		} `json:"target"`
		Group *struct {
			ID          string  `json:"id"`
			Path        string  `json:"path"`
			CurrentPath *string `json:"currentPath"`
		} `json:"group"`
		Before        *teamaudit.State `json:"before"`
		After         *teamaudit.State `json:"after"`
		Outcome       string           `json:"outcome"`
		ErrorCode     string           `json:"errorCode"`
		CorrelationID string           `json:"correlationId"`
	} `json:"items"`
	Next *string `json:"next"`
}

func readAudit(t *testing.T, got rawResponse) auditAnswer {
	t.Helper()
	if got.status != fiber.StatusOK {
		t.Fatalf("status %d (%s)", got.status, got.body)
	}
	var a auditAnswer
	if err := json.Unmarshal(got.body, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// The role alone decides, in every role mode: ADMIN, YK and DK read with
// it; ADMIN or DK without it, global:read, a Leader (S7), a member and a
// service account holding it do not. A filter the route cannot read is 400
// only for a reader. can["audit.read"] says the same.
func TestTeamAuditRecordsReaders(t *testing.T) {
	t.Parallel()
	for _, mode := range []authz.RoleMode{authz.RoleModeGroups, authz.RoleModeBoth, authz.RoleModeRoles} {
		e := newAuditReadEnv(t, mode)
		for _, tc := range []struct {
			name  string
			token string
			want  int
		}{
			{"ADMIN with audit:read", e.auditToken("admin", authz.RoleAuditRead), fiber.StatusOK},
			{"YK with audit:read", e.auditToken("yk", authz.RoleAuditRead), fiber.StatusOK},
			{"DK with audit:read", e.auditToken("dk", authz.RoleAuditRead, authz.RoleGlobalRead), fiber.StatusOK},
			{"ADMIN without it", e.auditToken("admin", authz.RoleGroupsManage, authz.RoleUsersManage), fiber.StatusForbidden},
			{"DK without it", e.auditToken("dk", authz.RoleGlobalRead), fiber.StatusForbidden},
			{"YK without it", e.auditToken("yk"), fiber.StatusForbidden},
			{"a Leader", e.auditToken("webLeader"), fiber.StatusForbidden},
			{"a Leader of a Sub-team", e.auditToken("frontLeader"), fiber.StatusForbidden},
			{"a member", e.auditToken("member"), fiber.StatusForbidden},
			{"a service account with audit:read", serviceToken(t, e.keys, "skymail", authz.RoleAuditRead), fiber.StatusForbidden},
		} {
			name := string(mode) + ", " + tc.name
			got := e.send(tc.token, fiber.MethodGet, auditRecordsPath, "")
			wantStatus(t, name, got, tc.want, "")
			if tc.want == fiber.StatusOK {
				if a := readAudit(t, got); a.Items == nil || a.Next != nil {
					t.Errorf("%s: %s", name, got.body)
				}
			}
			bad := e.send(tc.token, fiber.MethodGet, auditRecordsPath+"?actorId=nobody", "")
			wantBad := fiber.StatusForbidden
			if tc.want == fiber.StatusOK {
				wantBad = fiber.StatusBadRequest
			}
			wantStatus(t, name+", bad filter", bad, wantBad, "")
			if strings.HasPrefix(tc.name, "a service account") {
				continue
			}
			var caps authz.Capabilities
			if err := json.Unmarshal(e.send(tc.token, fiber.MethodGet, "/v1/users/me/capabilities", "").body, &caps); err != nil {
				t.Fatal(err)
			}
			if caps.Can["audit.read"] != (tc.want == fiber.StatusOK) {
				t.Errorf("%s: can[audit.read] = %v", name, caps.Can["audit.read"])
			}
		}
		wantStatus(t, string(mode)+", signed out", e.send("", fiber.MethodGet, auditRecordsPath, ""), fiber.StatusUnauthorized, "")
	}
}

// With delegation off the route answers as with it on: the record is read
// in both states (SPEC §6.1); a Leader is refused either way.
func TestTeamAuditRecordsDelegationOff(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{reads: true})
	wantStatus(t, "YK", e.send(e.auditToken("yk", authz.RoleAuditRead), fiber.MethodGet, auditRecordsPath, ""), fiber.StatusOK, "")
	wantStatus(t, "Leader", e.send(e.auditToken("webLeader"), fiber.MethodGet, auditRecordsPath, ""), fiber.StatusForbidden, "")
}

// Rows written through the routes come back with the names core holds now,
// an erased person as "Silinmiş kullanıcı", no address anywhere, the
// Group's path then and now (null once it is gone), never cached.
func TestTeamAuditRecordsAnswer(t *testing.T) {
	t.Parallel()
	e := newAuditReadEnv(t, authz.RoleModeGroups)
	front := "/UYELER/ARGE/WEBLAB/FRONT"
	wantStatus(t, "YK adds", e.add("yk", front, "newcomer"), fiber.StatusNoContent, "")
	wantStatus(t, "Leader adds", e.add("webLeader", front, "outsider"), fiber.StatusNoContent, "")
	wantStatus(t, "Leader refused", e.add("mobLeader", front, "exLeader"), fiber.StatusForbidden, "")
	// A Sub-team gone since: its row stays in WEBLAB's history. And
	// FRONT's move from another area: its row has the old path, and is
	// WEBLAB's history too, FRONT being under WEBLAB now.
	gone := uuid.NewString()
	ctx := context.Background()
	for _, r := range []teamaudit.Record{
		{Action: teamaudit.ActionTeamDelete, GroupID: gone, GroupPath: "/UYELER/ARGE/WEBLAB/GONE"},
		{Action: teamaudit.ActionTeamMove, GroupID: e.ids[front], GroupPath: "/UYELER/ORGANIZASYON/FRONT"},
	} {
		r.Actor = teamaudit.Actor{ID: e.people["yk"], Basis: teamaudit.BasisPrivileged, Client: "admin"}
		id, err := e.audit.Begin(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.audit.Finish(ctx, id, teamaudit.OutcomeOK, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.users.RequestDeletion(ctx, e.people["newcomer"], nil); err != nil {
		t.Fatal(err)
	}

	reader := e.auditToken("dk", authz.RoleAuditRead, authz.RoleGlobalRead)
	got := e.send(reader, fiber.MethodGet, auditRecordsPath, "")
	a := readAudit(t, got)
	for _, personal := range []string{"@example.test", "SKY-", "u-", "newcomer"} {
		if bytes.Contains(got.body, []byte(personal)) {
			t.Fatalf("the answer holds %q: %s", personal, got.body)
		}
	}
	if len(a.Items) != 5 {
		t.Fatalf("items %s", got.body)
	}
	byAction := map[string]int{}
	for i, item := range a.Items {
		if i > 0 && item.OccurredAt.After(a.Items[i-1].OccurredAt) {
			t.Fatalf("not newest first: %s", got.body)
		}
		byAction[item.Action+"/"+item.Outcome] = i
	}
	deleted := a.Items[byAction["team.delete/ok"]]
	if deleted.Group == nil || deleted.Group.ID != gone || deleted.Group.Path != "/UYELER/ARGE/WEBLAB/GONE" || deleted.Group.CurrentPath != nil ||
		deleted.Target != nil || deleted.Actor.DisplayName != "yk Kişi" {
		t.Fatalf("gone Group's row %+v", deleted)
	}
	moved := a.Items[byAction["team.move/ok"]]
	if moved.Group == nil || moved.Group.Path != "/UYELER/ORGANIZASYON/FRONT" || moved.Group.CurrentPath == nil || *moved.Group.CurrentPath != front {
		t.Fatalf("moved Group's row %+v", moved.Group)
	}
	refused := a.Items[byAction["member.add/denied"]]
	if refused.Actor.ID != e.people["mobLeader"] || refused.Actor.Basis != "none" || refused.ErrorCode != "forbidden" ||
		refused.Target == nil || refused.Target.DisplayName != "exLeader Kişi" {
		t.Fatalf("denied row %+v", refused)
	}
	added := []string{}
	for _, item := range a.Items {
		if item.Action != "member.add" || item.Outcome != "ok" {
			continue
		}
		if item.Group == nil || item.Group.ID != e.ids[front] || item.Group.Path != front || item.Group.CurrentPath == nil || *item.Group.CurrentPath != front {
			t.Fatalf("Group %+v", item.Group)
		}
		switch item.Actor.Basis {
		case "privileged":
			if item.Actor.DisplayName != "yk Kişi" || item.Target.DisplayName != user.DeletedDisplayName || item.Target.ID != e.people["newcomer"] {
				t.Fatalf("YK's add %+v %+v", item.Actor, item.Target)
			}
		case "leader":
			if item.Actor.DisplayName != "webLeader Kişi" || item.Actor.ViaGroupID != e.ids["/UYELER/ARGE/WEBLAB"] || item.Target.DisplayName != "outsider Kişi" {
				t.Fatalf("Leader's add %+v %+v", item.Actor, item.Target)
			}
		}
		added = append(added, item.Actor.Basis)
	}
	if slices.Sort(added); !slices.Equal(added, []string{"leader", "privileged"}) {
		t.Fatalf("adds %v", added)
	}

	resp := e.sendResponse(reader, auditRecordsPath)
	if resp != "no-store" {
		t.Fatalf("Cache-Control %q", resp)
	}

	// The filters: a subtree holds the gone Sub-team and FRONT; WEBLAB
	// alone holds nothing; an actor, a target, actions and an outcome.
	count := func(query url.Values) int {
		t.Helper()
		return len(readAudit(t, e.send(reader, fiber.MethodGet, auditRecordsPath+"?"+query.Encode(), "")).Items)
	}
	weblab := e.ids["/UYELER/ARGE/WEBLAB"]
	for _, tc := range []struct {
		query url.Values
		want  int
	}{
		{url.Values{"groupId": {weblab}, "subtree": {"true"}}, 5},
		{url.Values{"groupId": {e.ids[front]}}, 4},
		{url.Values{"groupId": {e.ids["/UYELER/ORGANIZASYON"]}, "subtree": {"true"}}, 1},
		{url.Values{"groupId": {weblab}}, 0},
		{url.Values{"groupId": {gone}, "subtree": {"true"}}, 1},
		{url.Values{"groupId": {e.ids["/UYELER/ARGE/MOBILAB"]}, "subtree": {"true"}}, 0},
		{url.Values{"actorId": {e.people["webLeader"].String()}}, 1},
		{url.Values{"userId": {e.people["newcomer"].String()}}, 1},
		{url.Values{"action": {"member.add,team.delete"}}, 4},
		{url.Values{"action": {"team.delete", "member.remove"}}, 1},
		{url.Values{"outcome": {"denied"}}, 1},
		{url.Values{"from": {time.Now().Add(time.Hour).Format(time.RFC3339)}}, 0},
		{url.Values{"to": {time.Now().Add(time.Hour).Format(time.RFC3339)}}, 5},
	} {
		if got := count(tc.query); got != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.query.Encode(), got, tc.want)
		}
	}
	for _, bad := range []string{
		"subtree=true", "groupId=WEBLAB", "subtree=yes", "action=member.invite", "outcome=maybe", "userId=x",
		"from=yesterday", "from=2026-10-31T10:00:00Z&to=2026-10-31T09:00:00Z", "limit=0", "limit=101", "limit=many", "cursor=xyz",
	} {
		wantStatus(t, bad, e.send(reader, fiber.MethodGet, auditRecordsPath+"?"+bad, ""), fiber.StatusBadRequest, "")
	}

	// Pages of one walk the five rows once; the last says next: null.
	seen := []uuid.UUID{}
	path := auditRecordsPath + "?limit=1"
	for range 10 {
		page := readAudit(t, e.send(reader, fiber.MethodGet, path, ""))
		for _, item := range page.Items {
			seen = append(seen, item.ID)
		}
		if page.Next == nil {
			break
		}
		path = auditRecordsPath + "?limit=1&cursor=" + url.QueryEscape(*page.Next)
	}
	want := []uuid.UUID{}
	for _, item := range a.Items {
		want = append(want, item.ID)
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("pages %v, want %v", seen, want)
	}
}

// sendResponse is the Cache-Control of a GET of path.
func (e *delegationEnv) sendResponse(token, path string) string {
	e.t.Helper()
	req := httptest.NewRequest(fiber.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.Header.Get(fiber.HeaderCacheControl)
}

// Every read is one log line: who, which kinds of filter and how it ended,
// never a filter's value. Not parallel: it reads the process-wide logger.
func TestTeamAuditRecordsAccessLog(t *testing.T) {
	e := newAuditReadEnv(t, authz.RoleModeGroups)
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	defer log.SetOutput(previous)

	target, group := e.people["member"].String(), e.ids["/UYELER/ARGE/WEBLAB"]
	query := "?groupId=" + group + "&subtree=true&userId=" + target + "&action=member.add&from=2026-01-02T03:04:05Z"
	wantStatus(t, "read", e.send(e.auditToken("yk", authz.RoleAuditRead), fiber.MethodGet, auditRecordsPath+query, ""), fiber.StatusOK, "")
	wantStatus(t, "refused", e.send(e.auditToken("webLeader"), fiber.MethodGet, auditRecordsPath+"?actorId="+target, ""), fiber.StatusForbidden, "")

	lines := []map[string]any{}
	for _, raw := range strings.Split(captured.String(), "\n") {
		if _, after, ok := strings.Cut(raw, "{"); ok && strings.Contains(raw, "team_audit_read") {
			line := map[string]any{}
			if err := json.Unmarshal([]byte("{"+after), &line); err != nil {
				t.Fatalf("line %q: %v", raw, err)
			}
			lines = append(lines, line)
		}
		for _, value := range []string{target, group, "member.add", "2026-01-02"} {
			if strings.Contains(raw, value) {
				t.Fatalf("log %q names %q", raw, value)
			}
		}
	}
	if len(lines) != 2 {
		t.Fatalf("lines %v", lines)
	}
	filters := func(line map[string]any) []string {
		out := []string{}
		for _, f := range line["filters"].([]any) {
			out = append(out, f.(string))
		}
		return out
	}
	if l := lines[0]; l["actor"] != e.people["yk"].String() || l["client"] != "admin" || l["outcome"] != "ok" ||
		!slices.Equal(filters(l), []string{"groupId", "subtree", "userId", "action", "from"}) {
		t.Fatalf("read line %v", l)
	}
	if l := lines[1]; l["actor"] != e.people["webLeader"].String() || l["outcome"] != "forbidden" || !slices.Equal(filters(l), []string{"actorId"}) {
		t.Fatalf("refused line %v", l)
	}
}
