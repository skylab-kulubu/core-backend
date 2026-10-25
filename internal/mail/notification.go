package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TicketCreatedType is the Notification type (and the e-mail template key) a
// new Ticket is told by (iletişim-servisi 06, docs/ticket-created-notification.md).
const TicketCreatedType = "event.ticket-created"

const kindTicketCreated = "ticket_created"

// Notification is one POST /v1/notifications (SkyMail docs/notifications.md):
// what happened and to whom; SkyMail picks the channels and the text.
type Notification struct {
	Type       string                  `json:"type"`
	Recipients []NotificationRecipient `json:"recipients"`
	Variables  map[string]string       `json:"variables,omitempty"`
	Source     *NotificationSource     `json:"source,omitempty"`
	// ExpiresAt is when what the notification is about ends: its inbox
	// items go then. Nil, or not in the future, leaves it out.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// NotificationRecipient is one person, by Keycloak subject. AllowedChannels
// is what their preferences allow; a required type does not read it, so it
// is left out there.
type NotificationRecipient struct {
	Sub             uuid.UUID `json:"sub"`
	AllowedChannels []string  `json:"allowed_channels,omitempty"`
}

// NotificationSource is where the notification came from.
type NotificationSource struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// Notify posts n under idempotencyKey (the same on every try of the same
// notification). It reports what SkyMail answered, as Membership does: the
// notification waits in core's queue until SkyMail has taken it. A refusal is
// warned about in the log with kind and no recipient.
func (s *SkyMail) Notify(ctx context.Context, kind, idempotencyKey string, n Notification) error {
	if s == nil || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	if n.ExpiresAt != nil && !n.ExpiresAt.After(time.Now()) {
		n.ExpiresAt = nil
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	body, err := json.Marshal(n)
	if err != nil {
		return &SendError{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/v1/notifications", bytes.NewReader(body))
	if err != nil {
		return &SendError{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return &SendError{}
	}
	defer resp.Body.Close()
	if succeeded(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	warn(s.Logger, eventCallFailed, kind, resp.StatusCode, sendReason(resp.StatusCode), diagnosticCode(diagnosticBody(resp.Body)))
	return &SendError{Status: resp.StatusCode}
}

// TicketCreated tells a new Ticket's holder: a registered holder by a
// Notification to their subject (inbox and e-mail, as SkyMail's type says),
// idempotent under idempotencyKey; a guest, who has no subject SkyMail could
// take, by the type's e-mail template alone (/v1/mail_tasks/single).
func (s *SkyMail) TicketCreated(ctx context.Context, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time) error {
	if holder.Sub != uuid.Nil {
		return s.Notify(ctx, kindTicketCreated, idempotencyKey, Notification{
			Type:       TicketCreatedType,
			Recipients: []NotificationRecipient{{Sub: holder.Sub}},
			Variables:  vars,
			Source:     &source,
			ExpiresAt:  expiresAt,
		})
	}
	if s == nil || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	status := s.post(ctx, kindTicketCreated, token, template{key: TicketCreatedType}.task(holder.Email, holder.FullName, vars))
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status}
}

// TicketHolder is whom a Ticket mail goes to: a registered holder's subject,
// or a guest's address and name.
type TicketHolder struct {
	Sub      uuid.UUID
	Email    string
	FullName string
}
