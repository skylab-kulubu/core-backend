package mail

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// The feedback mail posts by key with Items as a list of {Title, URL}, the
// contract of skymail-frontend's core.feedback-request.
func TestSkyMailFeedbackPostsItemsAsAList(t *testing.T) {
	t.Parallel()
	var payload struct {
		TemplateKey    string `json:"template_key"`
		RecipientEmail string `json:"recipient_email"`
		BodyVariables  struct {
			FirstName string           `json:"FirstName"`
			EventName string           `json:"EventName"`
			Items     []map[string]any `json:"Items"`
		} `json:"body_variables"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	sky := &SkyMail{BaseURL: srv.URL, FeedbackRequestTemplateKey: DefaultFeedbackRequestTemplateKey, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	items := []FeedbackItem{{Title: "Açılış", URL: "https://forms.yildizskylab.com/x?davet=a"}, {Title: "Go", URL: "https://forms.yildizskylab.com/y?davet=b"}}
	to := FeedbackRecipient{Email: "ada@example.test", FullName: "Ada Lovelace"}
	if err := sky.Feedback(t.Context(), FeedbackRequest, "key", to, uuid.New(), "Gecekodu", items); err != nil {
		t.Fatal(err)
	}
	b := payload.BodyVariables
	if payload.TemplateKey != "core.feedback-request" || payload.RecipientEmail != "ada@example.test" || b.FirstName != "" || b.EventName != "Gecekodu" ||
		len(b.Items) != 2 || b.Items[0]["Title"] != "Açılış" || b.Items[1]["URL"] != "https://forms.yildizskylab.com/y?davet=b" {
		t.Fatalf("payload %+v", payload)
	}
	off := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok")}
	if off.FeedbackConfigured(FeedbackRequest) || !errors.Is(off.Feedback(t.Context(), FeedbackRequest, "key", FeedbackRecipient{Email: "a@b.c"}, uuid.New(), "", nil), ErrTemplateUnconfigured) {
		t.Fatal("an empty key is on")
	}
}

// With FeedbackNotification on, a member's feedback mail is a Notification of
// the template's type (SkyMail sends it by e-mail and to the inbox, ticket 13):
// the member's subject with the address core read, the same variables and
// EventId for the inbox link, under the caller's Idempotency-Key. A guest,
// who has no subject, still gets the template by address.
func TestSkyMailFeedbackNotifiesAMember(t *testing.T) {
	t.Parallel()
	type call struct {
		path, key string
		body      map[string]any
	}
	calls := make(chan call, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		calls <- call{path: r.URL.Path, key: r.Header.Get("Idempotency-Key"), body: body}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	sky := &SkyMail{BaseURL: srv.URL, FeedbackReminderTemplateKey: DefaultFeedbackReminderTemplateKey, FeedbackNotification: true, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	sub, eventID := uuid.New(), uuid.New()
	items := []FeedbackItem{{Title: "Açılış", URL: "https://forms.yildizskylab.com/x?davet=a"}}

	member := FeedbackRecipient{Sub: sub, Email: "ada@example.test", FullName: "Ada Lovelace", FirstName: "Ada"}
	if err := sky.Feedback(t.Context(), FeedbackReminder, "core:feedback:k1", member, eventID, "Gecekodu", items); err != nil {
		t.Fatal(err)
	}
	got := <-calls
	if got.path != "/v1/notifications" || got.key != "core:feedback:k1" || got.body["type"] != "core.feedback-reminder" {
		t.Fatalf("member call %+v", got)
	}
	recipients, _ := got.body["recipients"].([]any)
	if len(recipients) != 1 {
		t.Fatalf("recipients %v", got.body["recipients"])
	}
	r := recipients[0].(map[string]any)
	if r["sub"] != sub.String() || r["email"] != "ada@example.test" || r["name"] != "Ada Lovelace" || r["allowed_channels"] != nil {
		t.Fatalf("recipient %v", r)
	}
	vars, _ := got.body["variables"].(map[string]any)
	list, _ := vars["Items"].([]any)
	if vars["FirstName"] != "Ada" || vars["EventName"] != "Gecekodu" || vars["EventId"] != eventID.String() || len(list) != 1 ||
		list[0].(map[string]any)["URL"] != "https://forms.yildizskylab.com/x?davet=a" {
		t.Fatalf("variables %v", vars)
	}
	if source, _ := got.body["source"].(map[string]any); source["kind"] != "event" || source["ref"] != eventID.String() {
		t.Fatalf("source %v", got.body["source"])
	}

	guest := FeedbackRecipient{Email: "guest@example.test", FullName: "Grace Hopper", FirstName: "Grace"}
	if err := sky.Feedback(t.Context(), FeedbackReminder, "core:feedback:k2", guest, eventID, "Gecekodu", items); err != nil {
		t.Fatal(err)
	}
	got = <-calls
	if got.path != "/v1/mail_tasks/single" || got.body["template_key"] != "core.feedback-reminder" || got.body["recipient_email"] != "guest@example.test" {
		t.Fatalf("guest call %+v", got)
	}
	if vars, _ := got.body["body_variables"].(map[string]any); vars["EventId"] != eventID.String() {
		t.Fatalf("guest variables %v", vars)
	}
}

// Off (the default), a member's feedback mail stays the template by address.
func TestSkyMailFeedbackOffMailsAMemberByAddress(t *testing.T) {
	t.Parallel()
	paths := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	sky := &SkyMail{BaseURL: srv.URL, FeedbackRequestTemplateKey: DefaultFeedbackRequestTemplateKey, Tokens: StaticToken("tok"), HTTP: srv.Client()}
	member := FeedbackRecipient{Sub: uuid.New(), Email: "ada@example.test"}
	if err := sky.Feedback(t.Context(), FeedbackRequest, "k", member, uuid.New(), "Gecekodu", nil); err != nil {
		t.Fatal(err)
	}
	if path := <-paths; path != "/v1/mail_tasks/single" {
		t.Fatalf("path %s", path)
	}
}
