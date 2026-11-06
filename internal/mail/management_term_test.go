package mail

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSkyMailManagementTermPostsByKey(t *testing.T) {
	t.Parallel()
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	sky := &SkyMail{BaseURL: srv.URL, ManagementTermTemplateKey: DefaultManagementTermTemplateKey, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	if !sky.ManagementTermConfigured() {
		t.Fatal("not configured")
	}
	if err := sky.ManagementTerm(t.Context(), "ada@example.test", "Ada Lovelace", map[string]string{"Notice": "warning"}); err != nil {
		t.Fatal(err)
	}
	if payload["template_key"] != "club.management-term" || payload["recipient_email"] != "ada@example.test" || payload["template_id"] != nil {
		t.Fatalf("payload %v", payload)
	}
}

func TestSkyMailManagementTermOffAndRefused(t *testing.T) {
	t.Parallel()
	off := &SkyMail{BaseURL: "http://skymail.invalid", ManagementTermTemplateKey: " ", Tokens: StaticToken("tok")}
	if off.ManagementTermConfigured() {
		t.Fatal("an empty key is on")
	}
	if err := off.ManagementTerm(t.Context(), "ada@example.test", "Ada", nil); !errors.Is(err, ErrTemplateUnconfigured) {
		t.Fatalf("err %v", err)
	}
	var nilSky *SkyMail
	if nilSky.ManagementTermConfigured() {
		t.Fatal("nil SkyMail is on")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	sky := &SkyMail{BaseURL: srv.URL, ManagementTermTemplateKey: "club.management-term", Tokens: StaticToken("tok"), HTTP: srv.Client(), Logger: log.New(&logs, "", 0)}
	err := sky.ManagementTerm(t.Context(), "ada@example.test", "Ada Lovelace", map[string]string{"Notice": "warning"})
	var refused *SendError
	if !errors.As(err, &refused) || refused.Status != http.StatusUnprocessableEntity || !refused.Permanent() {
		t.Fatalf("err %v", err)
	}
	if line := logs.String(); !strings.Contains(line, `"kind":"management_term"`) || strings.Contains(line, "ada@") || strings.Contains(line, "Ada") {
		t.Fatalf("log %q", line)
	}
}
