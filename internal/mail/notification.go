package mail

import (
	"bytes"
	"context"
	"encoding/base64"
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

// WaitlistJoinedType and WaitlistPromotedType are the Notification types
// (and e-mail template keys) of the waiting list (docs/event-capacity.md):
// "Bekleme listesindesin" and "Yer açıldı, biletin hazır". SkyMail seeds
// them (skymail-frontend, kontenjan-iptal-hatirlatma 09); core sends them
// only while TICKET_STATUS_MAIL is on.
const (
	WaitlistJoinedType   = "event.waitlist-joined"
	WaitlistPromotedType = "event.waitlist-promoted"
)

// TicketCancelledType is the Notification type (and e-mail template key) a
// Ticket's cancellation is told by (docs/ticket-status-mail.md): its holder
// cancelled it, or an operator did and chose to tell. SkyMail seeds it
// (skymail-frontend #179); core sends it only while TICKET_STATUS_MAIL is
// on.
const TicketCancelledType = "event.ticket-cancelled"

// EventUpdatedType is the Notification type (and the e-mail template key) a
// published Event's new time or place is told by (iletişim-servisi 06,
// docs/event-updated-notification.md).
const EventUpdatedType = "event.updated"

// EventChangedType is the Notification type (and the e-mail template key) a
// published Event's other details (name, description, attendee note,
// capacity, links) are told by when its editor says so
// (docs/event-updated-notification.md#other-details). SkyMail seeds it;
// core sends it only while EVENT_CHANGED_NOTIFICATION is on.
const EventChangedType = "event.changed"

// EventReminderType is the Notification type (and the e-mail template key)
// of the reminder the day before an Event (docs/event-reminder.md): the
// default of SKYMAIL_EVENT_REMINDER_TEMPLATE_KEY, and the type the
// Idempotency-Key names whatever key is set.
const EventReminderType = "event.reminder"

// The door's mails (docs/attendance-mail.md): the Notification types (and
// e-mail template keys) of "hoş geldin" on the day's first entry and of
// "katılımın alındı" on a Session Check-in, the defaults of
// SKYMAIL_ATTENDANCE_WELCOME_TEMPLATE_KEY and
// SKYMAIL_ATTENDANCE_RECORDED_TEMPLATE_KEY.
const (
	AttendanceWelcomeType  = "event.attendance-welcome"
	AttendanceRecordedType = "event.attendance-recorded"
)

// DoorStaffAddedType is the Notification type (and the e-mail template key)
// a person added to an Event's door staff is told by
// (docs/door-staff-mail.md). SkyMail seeds it with its template; core sends
// it only while DOOR_STAFF_MAIL is on.
const DoorStaffAddedType = "event.door-staff-added"

const (
	kindTicketCreated = "ticket_created"
	kindEventUpdated  = "event_updated"
	kindEventChanged  = "event_changed"
	kindTicketStatus  = "ticket_status"
	kindEventReminder = "event_reminder"
	kindAttendance    = "attendance_mail"
	kindDoorStaff     = "door_staff_added"
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
	// Attachments go with the e-mail of a Notification to one person
	// (SkyMail README "Posta eki"); none leaves the field out, so a body
	// without one is what it was before.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is one Mail attachment (ADR 0082; SkyMail README "Posta
// eki"): a PDF or PNG with a service's mail to one person, at most
// MaxAttachmentBytes decoded in all. SkyMail refuses anything else with
// 400 attachment.*.
type Attachment struct {
	// Filename is what the recipient sees: never the person's name.
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	// ContentBase64 is the file, standard base64 with padding.
	ContentBase64 string `json:"content_base64"`
}

// MaxAttachmentBytes is SkyMail's bound on a mail's attachments, decoded
// and together: 512 KB.
const MaxAttachmentBytes = 512 << 10

// PDFAttachment is data as a PDF attachment named filename.
func PDFAttachment(filename string, data []byte) Attachment {
	return Attachment{Filename: filename, ContentType: "application/pdf", ContentBase64: base64.StdEncoding.EncodeToString(data)}
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
	if s.noBaseURL() {
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
// attachments (the Ticket's PDF, docs/ticket-created-notification.md) go
// with the e-mail either way; none sends the body of before.
func (s *SkyMail) TicketCreated(ctx context.Context, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time, attachments []Attachment) error {
	return s.tellHolder(ctx, kindTicketCreated, TicketCreatedType, idempotencyKey, holder, vars, source, expiresAt, attachments)
}

// TicketStatus tells a Ticket's holder of its place on the waiting list
// (typ WaitlistJoinedType or WaitlistPromotedType) or of its cancellation
// (TicketCancelledType), the way TicketCreated tells of a new Ticket.
func (s *SkyMail) TicketStatus(ctx context.Context, typ, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time, attachments []Attachment) error {
	return s.tellHolder(ctx, kindTicketStatus, typ, idempotencyKey, holder, vars, source, expiresAt, attachments)
}

// EventUpdated tells a Ticket holder that the Event's time or place changed,
// the way TicketCreated tells of a new Ticket: a registered holder by a
// Notification to their subject, a guest by the type's e-mail template.
func (s *SkyMail) EventUpdated(ctx context.Context, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time) error {
	return s.tellHolder(ctx, kindEventUpdated, EventUpdatedType, idempotencyKey, holder, vars, source, expiresAt, nil)
}

// EventChanged tells a Ticket holder that the Event's other details
// changed, the way EventUpdated tells of a new time or place.
func (s *SkyMail) EventChanged(ctx context.Context, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time) error {
	return s.tellHolder(ctx, kindEventChanged, EventChangedType, idempotencyKey, holder, vars, source, expiresAt, nil)
}

// EventReminder tells a Ticket holder that the Event is tomorrow, under the
// template key typ (SKYMAIL_EVENT_REMINDER_TEMPLATE_KEY), the way
// EventUpdated tells of a new time or place.
func (s *SkyMail) EventReminder(ctx context.Context, typ, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time) error {
	return s.tellHolder(ctx, kindEventReminder, typ, idempotencyKey, holder, vars, source, expiresAt, nil)
}

// DoorStaffAdded tells a person added to an Event's door staff, by a
// Notification to their subject (inbox and e-mail, as SkyMail's type says),
// idempotent under idempotencyKey. Door staff are members: there is no
// guest form.
func (s *SkyMail) DoorStaffAdded(ctx context.Context, idempotencyKey string, sub uuid.UUID, vars map[string]string, source NotificationSource, expiresAt *time.Time) error {
	variables := make(map[string]any, len(vars))
	for name, value := range vars {
		variables[name] = value
	}
	return s.Notify(ctx, kindDoorStaff, idempotencyKey, Notification{
		Type:       DoorStaffAddedType,
		Recipients: []NotificationRecipient{{Sub: sub}},
		Variables:  variables,
		Source:     &source,
		ExpiresAt:  expiresAt,
	})
}

// tellHolder posts typ to a Ticket holder: a Notification to a registered
// holder's subject under idempotencyKey, the type's template by address to a
// guest. A guest's mail that went out with no answer is ErrMaybeDelivered.
func (s *SkyMail) tellHolder(ctx context.Context, kind, typ, idempotencyKey string, holder TicketHolder, vars map[string]string, source NotificationSource, expiresAt *time.Time, attachments []Attachment) error {
	variables := make(map[string]any, len(vars))
	for name, value := range vars {
		variables[name] = value
	}
	return s.tellHolderVariables(ctx, kind, typ, idempotencyKey, holder, variables, source, expiresAt, attachments)
}

// AttendanceMail tells a Ticket holder of their scan at the door under the
// template key typ (AttendanceWelcomeType's or AttendanceRecordedType's
// key; docs/attendance-mail.md), the way EventReminder tells of tomorrow.
// Its variables are not all text: the programme and the Sessions are lists,
// the counts numbers.
func (s *SkyMail) AttendanceMail(ctx context.Context, typ, idempotencyKey string, holder TicketHolder, vars map[string]any, source NotificationSource, expiresAt *time.Time) error {
	return s.tellHolderVariables(ctx, kindAttendance, typ, idempotencyKey, holder, vars, source, expiresAt, nil)
}

// tellHolderVariables is tellHolder with variables of any JSON shape.
func (s *SkyMail) tellHolderVariables(ctx context.Context, kind, typ, idempotencyKey string, holder TicketHolder, variables map[string]any, source NotificationSource, expiresAt *time.Time, attachments []Attachment) error {
	if holder.Sub != uuid.Nil {
		return s.Notify(ctx, kind, idempotencyKey, Notification{
			Type:        typ,
			Recipients:  []NotificationRecipient{{Sub: holder.Sub}},
			Variables:   variables,
			Source:      &source,
			ExpiresAt:   expiresAt,
			Attachments: attachments,
		})
	}
	if s.noBaseURL() {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	task := map[string]any{
		"template_key":        typ,
		"recipient_email":     holder.Email,
		"recipient_full_name": holder.FullName,
		"body_variables":      variables,
	}
	if len(attachments) > 0 {
		task["attachments"] = attachments
	}
	status, reached := s.postReached(ctx, kind, token, task)
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
