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

func TestSkyMailPrivilegeNoticePostsByKey(t *testing.T) {
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
	sky := &SkyMail{BaseURL: srv.URL, PrivilegeNoticeTemplateKey: DefaultPrivilegeNoticeTemplateKey, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	if !sky.PrivilegeNoticeConfigured() {
		t.Fatal("not configured")
	}
	if err := sky.PrivilegeNotice(t.Context(), "ada@example.test", "Ada Lovelace", map[string]string{"Change": "member-added"}); err != nil {
		t.Fatal(err)
	}
	if payload["template_key"] != "club.privilege-change" || payload["recipient_email"] != "ada@example.test" {
		t.Fatalf("payload %v", payload)
	}
}

func TestSkyMailPrivilegeNoticeOffAndRefused(t *testing.T) {
	t.Parallel()
	off := &SkyMail{BaseURL: "http://skymail.invalid", PrivilegeNoticeTemplateKey: "  ", Tokens: StaticToken("tok")}
	if off.PrivilegeNoticeConfigured() {
		t.Fatal("an empty key is on")
	}
	if err := off.PrivilegeNotice(t.Context(), "ada@example.test", "Ada", nil); !errors.Is(err, ErrTemplateUnconfigured) {
		t.Fatalf("err %v", err)
	}
	var nilSky *SkyMail
	if nilSky.PrivilegeNoticeConfigured() {
		t.Fatal("nil SkyMail is on")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	sky := &SkyMail{BaseURL: srv.URL, PrivilegeNoticeTemplateKey: "club.privilege-change", Tokens: StaticToken("tok"), HTTP: srv.Client(), Logger: log.New(&logs, "", 0)}
	err := sky.PrivilegeNotice(t.Context(), "ada@example.test", "Ada Lovelace", map[string]string{"GroupName": "YK"})
	var refused *SendError
	if !errors.As(err, &refused) || refused.Status != http.StatusNotFound || refused.Permanent() {
		t.Fatalf("err %v", err)
	}
	if line := logs.String(); !strings.Contains(line, `"kind":"privilege_notice"`) || strings.Contains(line, "ada@") || strings.Contains(line, "Ada") {
		t.Fatalf("log %q", line)
	}
}
