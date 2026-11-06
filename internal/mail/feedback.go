package mail

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// The SkyMail templates of the feedback mails (etkinlik geri bildirim
// formu, tickets 06 and 07; skymail-frontend core-feedback-*): variables
// FirstName (may be empty), EventName and Items, a list of {Title, URL}.
const (
	DefaultFeedbackRequestTemplateKey  = "core.feedback-request"
	DefaultFeedbackReminderTemplateKey = "core.feedback-reminder"
)

// FeedbackKind is one of the feedback mails.
type FeedbackKind string

const (
	FeedbackRequest  FeedbackKind = "request"
	FeedbackReminder FeedbackKind = "reminder"
)

// FeedbackItem is one form a feedback mail links: its title and the
// recipient's own link (the form address with the invite code, never a
// short link).
type FeedbackItem struct {
	Title string `json:"Title"`
	URL   string `json:"URL"`
}

// FeedbackRecipient is whom a feedback mail goes to: a member's subject
// (uuid.Nil for a guest), and the address and names the mail reads.
type FeedbackRecipient struct {
	Sub                        uuid.UUID
	Email, FullName, FirstName string
}

// FeedbackTemplateKey is kind's template key, empty when it is off.
func (s *SkyMail) FeedbackTemplateKey(kind FeedbackKind) string {
	if s == nil {
		return ""
	}
	switch kind {
	case FeedbackRequest:
		return strings.TrimSpace(s.FeedbackRequestTemplateKey)
	case FeedbackReminder:
		return strings.TrimSpace(s.FeedbackReminderTemplateKey)
	default:
		return ""
	}
}

// FeedbackConfigured reports whether kind's mail has a template key.
func (s *SkyMail) FeedbackConfigured(kind FeedbackKind) bool {
	return s.FeedbackTemplateKey(kind) != ""
}

// Feedback sends kind's mail and reports what SkyMail answered, as
// EventStatus does. Its variables are not all text: Items is a list, which
// SkyMail's templates range over; EventId fills the inbox item's link.
//
// With FeedbackNotification on, a member's mail is a Notification of the
// template's type (the type's e-mail text is the template of the same key,
// ADR 0070), under idempotencyKey: SkyMail sends it by e-mail and to the
// member's inbox. A guest has no subject SkyMail could take, so a guest's
// mail, and every mail while it is off, is the template by address
// (/v1/mail_tasks/single, which reads no key).
func (s *SkyMail) Feedback(ctx context.Context, kind FeedbackKind, idempotencyKey string, to FeedbackRecipient, eventID uuid.UUID, eventName string, items []FeedbackItem) error {
	key := s.FeedbackTemplateKey(kind)
	if key == "" || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	variables := map[string]any{
		"FirstName": to.FirstName,
		"EventName": eventName,
		"Items":     items,
		"EventId":   eventID.String(),
	}
	if s.FeedbackNotification && to.Sub != uuid.Nil {
		return s.Notify(ctx, "feedback_"+string(kind), idempotencyKey, Notification{
			Type:       key,
			Recipients: []NotificationRecipient{{Sub: to.Sub, Email: to.Email, Name: to.FullName}},
			Variables:  variables,
			Source:     &NotificationSource{Kind: "event", Ref: eventID.String()},
		})
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	task := map[string]any{
		"template_key":        key,
		"recipient_email":     to.Email,
		"recipient_full_name": to.FullName,
		"body_variables":      variables,
	}
	status := s.post(ctx, "feedback_"+string(kind), token, task)
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status}
}
