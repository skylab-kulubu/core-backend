package mail

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The publisher's mail goes by key to /single with its variables; an empty
// key, a nil mailer and no address send nothing.
func TestSkyMailCertificateAutoFinalized(t *testing.T) {
	t.Parallel()
	var gotPath string
	var payload map[string]any
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	mailer := &SkyMail{BaseURL: srv.URL, CertificateAutoFinalizedTemplateKey: DefaultCertificateAutoFinalizedTemplateKey, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	mailer.CertificateAutoFinalized(t.Context(), "ada@example.com", "Ada Lovelace", map[string]string{"EventName": "ARTLAB", "CertificateCount": "12"})
	if gotPath != "/v1/mail_tasks/single" || payload["template_key"] != "core.certificate-auto-finalized" || payload["recipient_email"] != "ada@example.com" {
		t.Fatalf("path %q payload %v", gotPath, payload)
	}
	if vars, _ := payload["body_variables"].(map[string]any); vars["CertificateCount"] != "12" || vars["EventName"] != "ARTLAB" {
		t.Fatalf("variables %v", payload["body_variables"])
	}

	off := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	off.CertificateAutoFinalized(t.Context(), "ada@example.com", "Ada", nil)
	mailer.CertificateAutoFinalized(t.Context(), " ", "Ada", nil)
	var none *SkyMail
	none.CertificateAutoFinalized(t.Context(), "ada@example.com", "Ada", nil)
	if posts != 1 {
		t.Fatalf("%d posts, want 1", posts)
	}
}
