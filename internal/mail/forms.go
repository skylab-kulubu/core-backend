package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// The template keys (and Notification types) of Forms' mails, the keys
// Forms sends today (forms-backend FormMail__*TemplateKey, SkyMail
// emails/CATALOG.md). core sends them once Forms' events reach it
// (ADR-0075 karar 5; docs/forms-mail.md); a run reopened goes as
// forms.status-changed with status "reopened", as Forms sends it.
const (
	FormsFormCopyType        = "forms.form-copy"
	FormsStatusChangedType   = "forms.status-changed"
	FormsAttemptUpdateType   = "forms.attempt-update"
	FormsPendingReminderType = "forms.pending-reminder"
)

const kindForms = "forms_mail"

// FormsRecipient is whom one of Forms' mails goes to: a member's subject,
// or a guest's address and full name.
type FormsRecipient struct {
	Sub      uuid.UUID
	Email    string
	FullName string
}

// FormsMail posts one of Forms' mails (typ, a Forms template key) under
// idempotencyKey, the same on every try. A member gets a Notification of
// the type (SkyMail registers the four as required, e-mail only: ADR-0075),
// a guest the type's template by address (/v1/mail_tasks/single) with the
// same Idempotency-Key header (skymail-backend #133): SkyMail answers a
// repeat with the first answer. A guest's mail that went out with no answer
// is ErrMaybeDelivered: the caller does not try it again, so a SkyMail that
// does not read the header yet cannot mail the guest twice. Its variables
// are not all text: answers and forms are lists.
func (s *SkyMail) FormsMail(ctx context.Context, typ, idempotencyKey string, to FormsRecipient, vars map[string]any, source NotificationSource) error {
	if to.Sub != uuid.Nil {
		return s.Notify(ctx, kindForms, idempotencyKey, Notification{
			Type:       typ,
			Recipients: []NotificationRecipient{{Sub: to.Sub}},
			Variables:  vars,
			Source:     &source,
		})
	}
	if s == nil || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	task := map[string]any{
		"template_key":        typ,
		"recipient_email":     to.Email,
		"recipient_full_name": to.FullName,
		"body_variables":      vars,
	}
	status, code, reached := s.postSingleWithKey(ctx, kindForms, token, idempotencyKey, task)
	switch {
	case succeeded(status):
		return nil
	case status == 0 && reached:
		return ErrMaybeDelivered
	}
	return &SendError{Status: status, Code: code}
}

// postSingleWithKey is postReached with an Idempotency-Key header, and the
// code of a refusal's body.
func (s *SkyMail) postSingleWithKey(ctx context.Context, kind, token, idempotencyKey string, task map[string]any) (int, string, bool) {
	body, err := json.Marshal(task)
	if err != nil {
		return 0, "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/v1/mail_tasks/single", bytes.NewReader(body))
	if err != nil {
		return 0, "", false
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
		return 0, "", !neverConnected(err)
	}
	defer resp.Body.Close()
	if succeeded(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, "", true
	}
	answer := diagnosticBody(resp.Body)
	warn(s.Logger, eventCallFailed, kind, resp.StatusCode, sendReason(resp.StatusCode), diagnosticCode(answer))
	return resp.StatusCode, answerCode(answer), true
}
