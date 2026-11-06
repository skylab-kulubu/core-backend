package mail

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSkyMailTeamPageNoticePostsByKey(t *testing.T) {
	t.Parallel()
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/mail_tasks/single" {
			t.Errorf("path %s", r.URL.Path)
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	sky := &SkyMail{BaseURL: srv.URL, TeamPageNoticeTemplateKey: DefaultTeamPageNoticeTemplateKey, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	if err := sky.TeamPageNotice(t.Context(), "ada@example.test", "Ada Lovelace", map[string]string{"Teams": "WEBLAB"}); err != nil {
		t.Fatal(err)
	}
	vars, _ := payload["body_variables"].(map[string]any)
	if payload["template_key"] != "core.team-page-consent-notice" || payload["recipient_email"] != "ada@example.test" || vars["Teams"] != "WEBLAB" {
		t.Fatalf("payload %v", payload)
	}
}

// Off, refused, and sent without an answer: only the last may have reached
// a mailbox, so the caller must not send it again.
func TestSkyMailTeamPageNoticeOffRefusedAndUnanswered(t *testing.T) {
	t.Parallel()
	off := &SkyMail{BaseURL: "http://skymail.invalid", TeamPageNoticeTemplateKey: " ", Tokens: StaticToken("tok")}
	if err := off.TeamPageNotice(t.Context(), "ada@example.test", "Ada", nil); !errors.Is(err, ErrTemplateUnconfigured) {
		t.Fatalf("off: %v", err)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(refusing.Close)
	sky := &SkyMail{BaseURL: refusing.URL, TeamPageNoticeTemplateKey: DefaultTeamPageNoticeTemplateKey, Tokens: StaticToken("tok"), Logger: log.New(io.Discard, "", 0)}
	var refused *SendError
	if err := sky.TeamPageNotice(t.Context(), "ada@example.test", "Ada", nil); !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
		t.Fatalf("refused: %v", err)
	}

	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(silent.Close)
	sky.BaseURL = silent.URL
	if err := sky.TeamPageNotice(t.Context(), "ada@example.test", "Ada", nil); !errors.Is(err, ErrMaybeDelivered) {
		t.Fatalf("no answer: %v", err)
	}
}
