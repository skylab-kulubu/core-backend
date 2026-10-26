package mail

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
	if err := sky.Feedback(t.Context(), FeedbackRequest, "ada@example.test", "Ada Lovelace", "", "Gecekodu", items); err != nil {
		t.Fatal(err)
	}
	b := payload.BodyVariables
	if payload.TemplateKey != "core.feedback-request" || payload.RecipientEmail != "ada@example.test" || b.FirstName != "" || b.EventName != "Gecekodu" ||
		len(b.Items) != 2 || b.Items[0]["Title"] != "Açılış" || b.Items[1]["URL"] != "https://forms.yildizskylab.com/y?davet=b" {
		t.Fatalf("payload %+v", payload)
	}
	off := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok")}
	if off.FeedbackConfigured(FeedbackRequest) || !errors.Is(off.Feedback(t.Context(), FeedbackRequest, "a@b.c", "", "", "", nil), ErrTemplateUnconfigured) {
		t.Fatal("an empty key is on")
	}
}
