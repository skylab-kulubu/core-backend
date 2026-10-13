package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSkyMailTeamMembershipPostsByKey(t *testing.T) {
	t.Parallel()
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mail_tasks/single" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("path %q auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	sky := &SkyMail{BaseURL: srv.URL, TeamMembershipTemplateKey: DefaultTeamMembershipTemplateKey, Tokens: StaticToken("tok"), HTTP: srv.Client()}

	err := sky.TeamMembership(t.Context(), "ada@example.test", "Ada Lovelace", map[string]string{"TeamName": "WEBLAB", "Action": "added"})
	if err != nil {
		t.Fatalf("TeamMembership: %v", err)
	}
	if payload["template_key"] != "club.team-membership" || payload["template_id"] != nil {
		t.Fatalf("addressing %v / %v", payload["template_key"], payload["template_id"])
	}
	if payload["recipient_email"] != "ada@example.test" || payload["recipient_full_name"] != "Ada Lovelace" {
		t.Fatalf("recipient %v %v", payload["recipient_email"], payload["recipient_full_name"])
	}
	vars, _ := payload["body_variables"].(map[string]any)
	if vars["TeamName"] != "WEBLAB" || vars["Action"] != "added" {
		t.Fatalf("vars %v", vars)
	}
}

// Unlike the fire-and-forget sends, the team membership mail waits in core's
// queue until SkyMail took it, so the caller hears what SkyMail answered.
func TestSkyMailTeamMembershipReportsRefusalWithoutPII(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status    int
		permanent bool
	}{
		{http.StatusNotFound, false},
		{http.StatusServiceUnavailable, false},
		{http.StatusTooManyRequests, false},
		{http.StatusUnauthorized, false},
		{http.StatusBadRequest, true},
		{http.StatusUnprocessableEntity, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"template_not_found"}`))
		}))
		var logs bytes.Buffer
		sky := &SkyMail{
			BaseURL: srv.URL, TeamMembershipTemplateKey: "club.team-membership", Tokens: StaticToken("tok"),
			HTTP: srv.Client(), Logger: log.New(&logs, "", 0),
		}
		err := sky.TeamMembership(t.Context(), "ada@example.test", "Ada Lovelace", map[string]string{"TeamName": "WEBLAB"})
		srv.Close()
		var refused *SendError
		if !errors.As(err, &refused) || refused.Status != tc.status || refused.Permanent() != tc.permanent {
			t.Fatalf("status %d: err %v", tc.status, err)
		}
		line := logs.String()
		if !strings.Contains(line, `"kind":"team_membership"`) || strings.Contains(line, "ada@") || strings.Contains(line, "Ada") || strings.Contains(line, "WEBLAB") {
			t.Fatalf("status %d: log %q", tc.status, line)
		}
		if strings.Contains(err.Error(), "ada@") {
			t.Fatalf("error names the recipient: %v", err)
		}
	}
}

func TestSkyMailTeamMembershipTransportAndTokenFailuresAreRetryable(t *testing.T) {
	t.Parallel()
	sky := &SkyMail{BaseURL: "http://127.0.0.1:1", TeamMembershipTemplateKey: "club.team-membership", Tokens: StaticToken("tok")}
	err := sky.TeamMembership(t.Context(), "ada@example.test", "", nil)
	var refused *SendError
	if !errors.As(err, &refused) || refused.Status != 0 || refused.Permanent() {
		t.Fatalf("transport: %v", err)
	}

	sky = &SkyMail{BaseURL: "http://127.0.0.1:1", TeamMembershipTemplateKey: "club.team-membership", Tokens: failingToken{}}
	if err := sky.TeamMembership(t.Context(), "ada@example.test", "", nil); !errors.As(err, &refused) || refused.Permanent() {
		t.Fatalf("token: %v", err)
	}
}

func TestSkyMailTeamMembershipOffWithoutKey(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)
	sky := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	if sky.TeamMembershipConfigured() {
		t.Fatal("configured without a key")
	}
	if err := sky.TeamMembership(t.Context(), "ada@example.test", "", nil); !errors.Is(err, ErrTemplateUnconfigured) || called {
		t.Fatalf("err %v called %v", err, called)
	}
	var nilSky *SkyMail
	if nilSky.TeamMembershipConfigured() {
		t.Fatal("nil SkyMail configured")
	}
}

type failingToken struct{}

func (failingToken) Token(context.Context) (string, error) { return "", errors.New("no token") }

// Each kind of membership mail (docs/team-membership-mail.md) posts its own
// template key and names its own kind in a refusal's log line.
func TestSkyMailMembershipKindsPostTheirOwnTemplate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind    MembershipKind
		key     string
		logKind string
	}{
		{MembershipTeam, "club.team-membership", "team_membership"},
		{MembershipRole, "club.role-membership", "role_membership"},
		{MembershipGroup, "club.group-membership", "group_membership"},
	} {
		var payload map[string]any
		status := http.StatusCreated
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)
			w.WriteHeader(status)
		}))
		var logs bytes.Buffer
		sky := &SkyMail{
			BaseURL:                    srv.URL,
			TeamMembershipTemplateKey:  DefaultTeamMembershipTemplateKey,
			RoleMembershipTemplateKey:  DefaultRoleMembershipTemplateKey,
			GroupMembershipTemplateKey: DefaultGroupMembershipTemplateKey,
			Tokens:                     StaticToken("tok"), HTTP: srv.Client(), Logger: log.New(&logs, "", 0),
		}
		if !sky.MembershipConfigured(tc.kind) {
			t.Fatalf("%s: not configured", tc.kind)
		}
		if err := sky.Membership(t.Context(), tc.kind, "ada@example.test", "Ada Lovelace", map[string]string{"GroupName": "YK"}); err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if payload["template_key"] != tc.key {
			t.Fatalf("%s: key %v", tc.kind, payload["template_key"])
		}
		status = http.StatusNotFound
		err := sky.Membership(t.Context(), tc.kind, "ada@example.test", "Ada Lovelace", nil)
		srv.Close()
		var refused *SendError
		if !errors.As(err, &refused) || refused.Status != http.StatusNotFound || refused.Permanent() {
			t.Fatalf("%s: refusal %v", tc.kind, err)
		}
		if !strings.Contains(logs.String(), `"kind":"`+tc.logKind+`"`) || strings.Contains(logs.String(), "ada@") {
			t.Fatalf("%s: log %q", tc.kind, logs.String())
		}
	}
}

// A kind without a key is off on its own: the others still send.
func TestSkyMailMembershipKindOffWithoutItsKey(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	sky := &SkyMail{BaseURL: srv.URL, TeamMembershipTemplateKey: DefaultTeamMembershipTemplateKey, GroupMembershipTemplateKey: " ", Tokens: StaticToken("tok"), HTTP: srv.Client()}
	for _, kind := range []MembershipKind{MembershipRole, MembershipGroup, MembershipKind("other")} {
		if sky.MembershipConfigured(kind) {
			t.Fatalf("%s configured", kind)
		}
		if err := sky.Membership(t.Context(), kind, "ada@example.test", "", nil); !errors.Is(err, ErrTemplateUnconfigured) || called {
			t.Fatalf("%s: err %v called %v", kind, err, called)
		}
	}
	if !sky.MembershipConfigured(MembershipTeam) {
		t.Fatal("team off")
	}
	var nilSky *SkyMail
	if nilSky.MembershipConfigured(MembershipTeam) {
		t.Fatal("nil SkyMail configured")
	}
}
