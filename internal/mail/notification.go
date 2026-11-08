package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TicketCreatedType is the Notification type (and the e-mail template key) a
// new Ticket is told by (iletişim-servisi 06, docs/ticket-created-notification.md).
const TicketCreatedType = "event.ticket-created"

// EventUpdatedType is the Notification type (and the e-mail template key) a
// published Event's new time or place is told by (iletişim-servisi 06,
// docs/event-updated-notification.md).
const EventUpdatedType = "event.updated"

const (
	kindTicketCreated = "ticket_created"
	kindEventUpdated  = "event_updated"
)

// Notification is one POST /v1/notifications (SkyMail docs/notifications.md):
// what happened and to whom; SkyMail picks the channels and the text.
type Notification struct {
	Type       string                  `json:"type"`
	Recipients []NotificationRecipient `json:"recipients"`
	// Variables are the same for every recipient; a feedback mail's Items is
	// a list, the rest are text.
	Variables map[string]any      `json:"variables,omitempty"`
	Source    *NotificationSource `json:"source,omitempty"`
	// ExpiresAt is when what the notification is about ends: its inbox
	// items go then. Nil, or not in the future, leaves it out.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// NotificationRecipient is one person, by Keycloak subject. AllowedChannels
// is what their preferences allow: nil leaves it out (a required type does
// not read it), a pointer to an empty list sends [] (an optional type the
// person allows no channel of; SkyMail refuses an optional type without the
// field).
type NotificationRecipient struct {
	Sub uuid.UUID `json:"sub"`
	// Email and Name, when given, are the address and name core read for the
	// person (SkyMail then does not look them up in Keycloak).
	Email           string    `json:"email,omitempty"`
	Name            string    `json:"name,omitempty"`
	AllowedChannels *[]string `json:"allowed_channels,omitempty"`
}

// ErrMaybeDelivered is a send that went out and got no answer: SkyMail may
// have taken it. The guest's mail has no idempotency key (/single reads none),
// so it is not tried again: a second try could mail the guest twice.
var ErrMaybeDelivered = errors.New("mail: SkyMail may have taken the mail; no answer came")

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
	answer := diagnosticBody(resp.Body)
	warn(s.Logger, eventCallFailed, kind, resp.StatusCode, sendReason(resp.StatusCode), diagnosticCode(answer))
	return &SendError{Status: resp.StatusCode, Code: answerCode(answer)}
}

// answerCode is the code of SkyMail's error body ({"code": …}, its
// AppError), else what diagnosticCode reads; empty when nothing code-shaped
// is there.
func answerCode(body []byte) string {
	if len(body) == 0 || len(body) > maxDiagnosticBody {
		return ""
	}
	var fields struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &fields) == nil && codeShaped(fields.Code) {
		return fields.Code
	}
	return diagnosticCode(body)
}

// TicketCreated tells a new Ticket's holder: a registered holder by a
// Notification to their subject (inbox and e-mail, as SkyMail's type says),
// idempotent under idempotencyKey; a guest, who has no subject SkyMail could
// take, by the type's e-mail template alone (/v1/mail_tasks/single).
func (s *SkyMail) TicketCreated(ctx context.Context, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time) error {
	return s.tellHolder(ctx, kindTicketCreated, TicketCreatedType, idempotencyKey, holder, vars, source, expiresAt)
}

// EventUpdated tells a Ticket holder that the Event's time or place changed,
// the way TicketCreated tells of a new Ticket: a registered holder by a
// Notification to their subject, a guest by the type's e-mail template.
func (s *SkyMail) EventUpdated(ctx context.Context, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time) error {
	return s.tellHolder(ctx, kindEventUpdated, EventUpdatedType, idempotencyKey, holder, vars, source, expiresAt)
}

// tellHolder posts typ to a Ticket holder: a Notification to a registered
// holder's subject under idempotencyKey, the type's template by address to a
// guest. A guest's mail that went out with no answer is ErrMaybeDelivered.
func (s *SkyMail) tellHolder(ctx context.Context, kind, typ, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time) error {
	if holder.Sub != uuid.Nil {
		variables := make(map[string]any, len(vars))
		for name, value := range vars {
			variables[name] = value
		}
		return s.Notify(ctx, kind, idempotencyKey, Notification{
			Type:       typ,
			Recipients: []NotificationRecipient{{Sub: holder.Sub}},
			Variables:  variables,
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
	status, reached := s.postReached(ctx, kind, token, template{key: typ}.task(holder.Email, holder.FullName, vars))
	switch {
	case succeeded(status):
		return nil
	case status == 0 && reached:
		return ErrMaybeDelivered
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
