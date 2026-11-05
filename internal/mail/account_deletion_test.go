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

// A deletion notice goes by key, with no name and with erase_after_send, and
// counts as taken only when SkyMail says it keeps no record of it.
func TestAccountDeletionNoticeAsksSkyMailToKeepNoRecord(t *testing.T) {
	t.Parallel()
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/mail_tasks/single" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"0b6f2a3e-8d1c-4f5e-9a7b-3c2d1e0f9a8b","tracking":false,"erase_after_send":true}`))
	}))
	t.Cleanup(srv.Close)
	sky := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	if err := sky.AccountDeletionNotice(t.Context(), DefaultAccountDeletionReceivedTemplateKey, "ada@example.test", map[string]string{"Deadline": "30.11.2026"}); err != nil {
		t.Fatal(err)
	}
	vars, _ := payload["body_variables"].(map[string]any)
	if payload["template_key"] != "account.deletion-requested" || payload["recipient_email"] != "ada@example.test" ||
		payload["recipient_full_name"] != "" || payload["erase_after_send"] != true || vars["Deadline"] != "30.11.2026" {
		t.Fatalf("payload %v", payload)
	}
	if _, ok := payload["template_id"]; ok {
		t.Fatalf("payload %v carries a template id", payload)
	}
	if _, ok := payload["tracking"]; ok {
		t.Fatalf("payload %v asks about tracking", payload)
	}
}

// SkyMail from before erase_after_send takes the mail and says nothing of
// it; a refusal is a SendError. Neither line names the recipient.
func TestAccountDeletionNoticeUnconfirmedRefusedAndOff(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)

	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"0b6f2a3e-8d1c-4f5e-9a7b-3c2d1e0f9a8b","tracking":false}`))
	}))
	t.Cleanup(old.Close)
	sky := &SkyMail{BaseURL: old.URL, Tokens: StaticToken("tok"), HTTP: old.Client(), Logger: logger}
	if err := sky.AccountDeletionNotice(t.Context(), DefaultAccountDeletionCompletedTemplateKey, "ada@example.test", nil); !errors.Is(err, ErrEraseAfterSendUnconfirmed) {
		t.Fatalf("old SkyMail: %v", err)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"ada@example.test"}`))
	}))
	t.Cleanup(refusing.Close)
	sky = &SkyMail{BaseURL: refusing.URL, Tokens: StaticToken("tok"), HTTP: refusing.Client(), Logger: logger}
	var refused *SendError
	if err := sky.AccountDeletionNotice(t.Context(), DefaultAccountDeletionCompletedTemplateKey, "ada@example.test", nil); !errors.As(err, &refused) || refused.Status != http.StatusServiceUnavailable {
		t.Fatalf("refused: %v", err)
	}
	if strings.Contains(logs.String(), "ada") {
		t.Fatalf("log names the recipient: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"reason":"erase_after_send_unconfirmed"`) || !strings.Contains(logs.String(), `"status":503`) {
		t.Fatalf("log %s", logs.String())
	}

	off := &SkyMail{BaseURL: refusing.URL, Tokens: StaticToken("tok")}
	if err := off.AccountDeletionNotice(t.Context(), " ", "ada@example.test", nil); !errors.Is(err, ErrTemplateUnconfigured) {
		t.Fatalf("no key: %v", err)
	}
}
