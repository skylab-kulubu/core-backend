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
	"time"

	"github.com/google/uuid"
)

type captured struct {
	path, key, auth string
	body            map[string]any
}

func notificationServer(t *testing.T, status int, answer string) (*httptest.Server, *[]captured) {
	t.Helper()
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body: %v", err)
		}
		got = append(got, captured{r.URL.Path, r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization"), body})
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestTicketCreatedForAMemberPostsANotification(t *testing.T) {
	srv, got := notificationServer(t, http.StatusCreated, `{}`)
	sky := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok")}
	sub := uuid.New()
	end := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	err := sky.TicketCreated(context.Background(), "core:event.ticket-created:x:v1", TicketHolder{Sub: sub, Email: "unused@example.com"},
		map[string]string{"EventName": "Gece"}, NotificationSource{Kind: "ticket", Ref: "x"}, &end)
	if err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("requests %d", len(*got))
	}
	c := (*got)[0]
	if c.path != "/v1/notifications" || c.key != "core:event.ticket-created:x:v1" || c.auth != "Bearer tok" {
		t.Fatalf("request %+v", c)
	}
	if c.body["type"] != TicketCreatedType {
		t.Fatalf("type %v", c.body["type"])
	}
	recipients, _ := c.body["recipients"].([]any)
	if len(recipients) != 1 {
		t.Fatalf("recipients %v", c.body["recipients"])
	}
	r := recipients[0].(map[string]any)
	// A required type: no allowed_channels, and no address (SkyMail reads
	// the Primary e-mail from Keycloak).
	if r["sub"] != sub.String() || len(r) != 1 {
		t.Fatalf("recipient %v, want the subject alone", r)
	}
	if c.body["expires_at"] != end.Format(time.RFC3339) {
		t.Fatalf("expires_at %v, want %s", c.body["expires_at"], end.Format(time.RFC3339))
	}
	if src, _ := c.body["source"].(map[string]any); src["kind"] != "ticket" || src["ref"] != "x" {
		t.Fatalf("source %v", c.body["source"])
	}
}

func TestTicketCreatedForAGuestPostsTheTemplateByAddress(t *testing.T) {
	srv, got := notificationServer(t, http.StatusCreated, `{}`)
	sky := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok")}
	if err := sky.TicketCreated(context.Background(), "k", TicketHolder{Email: "can@example.com", FullName: "Can Er"},
		map[string]string{"EventName": "Gece"}, NotificationSource{Kind: "ticket", Ref: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	c := (*got)[0]
	if c.path != "/v1/mail_tasks/single" || c.body["template_key"] != TicketCreatedType ||
		c.body["recipient_email"] != "can@example.com" || c.body["recipient_full_name"] != "Can Er" {
		t.Fatalf("request %+v", c)
	}
}

func TestNotifyReportsRefusalsAndLogsNoPerson(t *testing.T) {
	srv, _ := notificationServer(t, http.StatusUnprocessableEntity, `{"error":"notification.type_unknown"}`)
	var logs bytes.Buffer
	sky := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok"), Logger: log.New(&logs, "", 0)}
	past := time.Now().Add(-time.Hour)
	err := sky.Notify(context.Background(), "ticket_created", "k", Notification{Type: TicketCreatedType, Recipients: []NotificationRecipient{{Sub: uuid.New()}}, ExpiresAt: &past})
	var refused *SendError
	if !errors.As(err, &refused) || refused.Status != http.StatusUnprocessableEntity || !refused.Permanent() {
		t.Fatalf("err %v, want a permanent 422", err)
	}
	line := logs.String()
	if !strings.Contains(line, `"kind":"ticket_created"`) || !strings.Contains(line, `"code":"notification.type_unknown"`) {
		t.Fatalf("log %q", line)
	}
}

func TestNotifyLeavesAPastExpiryOut(t *testing.T) {
	srv, got := notificationServer(t, http.StatusCreated, `{}`)
	sky := &SkyMail{BaseURL: srv.URL, Tokens: StaticToken("tok")}
	past := time.Now().Add(-time.Hour)
	if err := sky.Notify(context.Background(), "ticket_created", "k", Notification{Type: TicketCreatedType, Recipients: []NotificationRecipient{{Sub: uuid.New()}}, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if _, ok := (*got)[0].body["expires_at"]; ok {
		t.Fatal("a past expiry must not be sent: SkyMail refuses it")
	}
}

func TestNotifyWithoutSkyMailIsUnconfigured(t *testing.T) {
	var sky *SkyMail
	if err := sky.Notify(context.Background(), "k", "k", Notification{}); !errors.Is(err, ErrTemplateUnconfigured) {
		t.Fatalf("err %v", err)
	}
}
