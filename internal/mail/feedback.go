package mail

import (
	"context"
	"strings"
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

// Feedback posts kind's mail by template key and reports what SkyMail
// answered, as EventStatus does. Its variables are not all text: Items is a
// list, which SkyMail's templates range over.
func (s *SkyMail) Feedback(ctx context.Context, kind FeedbackKind, recipient, fullName, firstName, eventName string, items []FeedbackItem) error {
	key := s.FeedbackTemplateKey(kind)
	if key == "" || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	task := map[string]any{
		"template_key":        key,
		"recipient_email":     recipient,
		"recipient_full_name": fullName,
		"body_variables": map[string]any{
			"FirstName": firstName,
			"EventName": eventName,
			"Items":     items,
		},
	}
	status := s.post(ctx, "feedback_"+string(kind), token, task)
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status}
}
