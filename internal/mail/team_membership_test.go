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
