package mail

import (
	"context"
	"strings"
)

// The SkyMail templates of the Event status mails (ticket 14): a cancelled
// Event's Ticket holders are told of it, and of the cancellation undone.
const (
	DefaultEventCancelledTemplateKey  = "core.event-cancelled"
	DefaultEventReinstatedTemplateKey = "core.event-reinstated"
)

// EventStatusKind is one of the Event status mails.
type EventStatusKind string

const (
	EventStatusCancelled  EventStatusKind = "cancelled"
	EventStatusReinstated EventStatusKind = "reinstated"
)

// EventStatusTemplateKey is kind's template key, empty when it is off.
func (s *SkyMail) EventStatusTemplateKey(kind EventStatusKind) string {
	if s == nil {
		return ""
	}
	switch kind {
	case EventStatusCancelled:
		return strings.TrimSpace(s.EventCancelledTemplateKey)
	case EventStatusReinstated:
		return strings.TrimSpace(s.EventReinstatedTemplateKey)
	default:
		return ""
	}
}

// EventStatusConfigured reports whether kind's mail has a template key.
func (s *SkyMail) EventStatusConfigured(kind EventStatusKind) bool {
	return s.EventStatusTemplateKey(kind) != ""
}

// EventStatus posts kind's mail by template key and reports what SkyMail
// answered, as Membership does: the mail waits in core's queue until
// SkyMail has taken it (internal/eventstatusmail). A refusal is warned
// about in the log with the kind event_cancelled or event_reinstated and
// no recipient.
func (s *SkyMail) EventStatus(ctx context.Context, kind EventStatusKind, recipient, fullName string, vars map[string]string) error {
	key := s.EventStatusTemplateKey(kind)
	if key == "" || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	status := s.post(ctx, "event_"+string(kind), token, template{key: key}.task(recipient, fullName, vars))
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status}
}
