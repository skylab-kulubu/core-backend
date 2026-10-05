package mail

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSkyMailConsentConfirmationPostsByKey(t *testing.T) {
	t.Parallel()
	var gotPath string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	mailer := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	mailer.ConsentConfirmation(t.Context(), "core.contact-consent-confirm", "ada@example.com", map[string]string{
		"confirmUrl": "https://api.example.test/v1/consents/confirm?token=x",
	})
	if gotPath != "/v1/mail_tasks/single" {
		t.Fatalf("path %q", gotPath)
	}
	if payload["template_key"] != "core.contact-consent-confirm" || payload["recipient_email"] != "ada@example.com" || payload["template_id"] != nil {
		t.Fatalf("payload %v", payload)
	}
	// Whoever typed the address may have typed any name: none is sent.
	if name, _ := payload["recipient_full_name"].(string); name != "" {
		t.Fatalf("the confirmation mail carries a name: %q", name)
	}
	vars, _ := payload["body_variables"].(map[string]any)
	if vars["confirmUrl"] != "https://api.example.test/v1/consents/confirm?token=x" {
		t.Fatalf("vars %v", vars)
	}
}

func TestSkyMailConsentConfirmationNoopsWithoutKey(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)
	mailer := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	mailer.ConsentConfirmation(t.Context(), " ", "ada@example.com", nil)
	if called {
		t.Fatal("posted without a template key")
	}
}
