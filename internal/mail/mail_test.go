package mail

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestSkyMailWelcomePostsSingleTask(t *testing.T) {
	t.Parallel()
	templateID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"t1"}`))
	}))
	t.Cleanup(srv.Close)

	mailer := &SkyMail{
		BaseURL:    srv.URL,
		TemplateID: templateID,
		Tokens:     StaticToken("tok"),
		HTTP:       srv.Client(),
	}
	mailer.Welcome(t.Context(), user.User{
		Email:     "ada@example.com",
		FirstName: "Ada",
		LastName:  "Lovelace",
		CreatedAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
	})
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth %q", gotAuth)
	}
	if gotPath != "/v1/mail_tasks/single" {
		t.Fatalf("path %q", gotPath)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["template_id"] != templateID.String() {
		t.Fatalf("template %v", payload["template_id"])
	}
	if payload["recipient_email"] != "ada@example.com" {
		t.Fatalf("email %v", payload["recipient_email"])
	}
	if payload["recipient_full_name"] != "Ada Lovelace" {
		t.Fatalf("name %v", payload["recipient_full_name"])
	}
}

func TestSkyMailWelcomeNoopsWithoutTemplate(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	mailer := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	mailer.Welcome(t.Context(), user.User{Email: "a@b.c"})
	if called {
		t.Fatal("should not post")
	}
}

func TestSkyMailCertificatePostsSingleTask(t *testing.T) {
	t.Parallel()
	templateID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	var gotPath string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	mailer := &SkyMail{
		BaseURL:               srv.URL,
		CertificateTemplateID: templateID,
		Tokens:                StaticToken("tok"),
		HTTP:                  srv.Client(),
	}
	mailer.Certificate(t.Context(), "ada@example.com", "Ada Lovelace", map[string]string{"EventName": "ARTLAB"})
	if gotPath != "/v1/mail_tasks/single" {
		t.Fatalf("path %q", gotPath)
	}
	if payload["template_id"] != templateID.String() {
		t.Fatalf("template %v", payload["template_id"])
	}
	if payload["recipient_email"] != "ada@example.com" {
		t.Fatalf("email %v", payload["recipient_email"])
	}
}

func TestSkyMailCertificateNoopsWithoutTemplate(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	mailer := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	mailer.Certificate(t.Context(), "ada@example.com", "Ada", nil)
	if called {
		t.Fatal("should not post")
	}
}

func TestAPIOriginDefaultsToPublicSkymailAPI(t *testing.T) {
	t.Parallel()
	if got := APIOrigin(""); got != "https://api.yildizskylab.com/api/skymail" {
		t.Fatalf("empty %q", got)
	}
	if got := APIOrigin("  "); got != "https://api.yildizskylab.com/api/skymail" {
		t.Fatalf("blank %q", got)
	}
	if got := APIOrigin("https://mail.example.test/"); got != "https://mail.example.test" {
		t.Fatalf("override %q", got)
	}
}
