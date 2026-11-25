package mail

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

func (s *SkyNotify) Certificate(ctx context.Context, recipientEmail, fullName string, vars map[string]string) {
	if strings.TrimSpace(recipientEmail) == "" {
		return
	}
	s.send(ctx, kindCertificate, s.certificateTemplate(), recipientEmail, fullName, vars)
}

// CertificateMail posts the certificate mail under idempotencyKey, the same
// on every try of one queued mail (docs/certificate-mail.md): SkyNotify answers
// a repeat with its first answer, so core's queue tries again whenever no
// answer came. It reports what SkyNotify answered: nil once it took the mail,
// ErrTemplateUnconfigured when there is no address or template, else a
// *SendError (Status 0: SkyNotify was not reached).
//
// A template key SkyNotify does not know (404) falls back to the template id,
// as Certificate does, under the key with ":id" appended: the body differs,
// and SkyNotify refuses one key for two bodies.
func (s *SkyNotify) CertificateMail(ctx context.Context, idempotencyKey, recipientEmail, fullName string, vars map[string]string) error {
	return s.CertificateMailAttachments(ctx, idempotencyKey, recipientEmail, fullName, vars, nil)
}

// CertificateMailAttachments is CertificateMail with the certificate's PDF
// (CERTIFICATE_PDF_ATTACHMENT; docs/certificate-mail.md): attachments go in
// the body's "attachments" (SkyNotify README "Posta eki"), none leaves the
// field out. SkyNotify keeps them out of the Idempotency-Key's digest, so a
// try with the file and one without are the same mail.
func (s *SkyNotify) CertificateMailAttachments(ctx context.Context, idempotencyKey, recipientEmail, fullName string, vars map[string]string, attachments []Attachment) error {
	if s.noBaseURL() {
		return ErrTemplateUnconfigured
	}
	tpl := s.certificateTemplate()
	if !tpl.configured() {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	if tpl.key != "" {
		status, _, code := s.postTask(ctx, kindCertificate, token, idempotencyKey, withAttachments(template{key: tpl.key}.task(recipientEmail, fullName, vars), attachments))
		if succeeded(status) {
			return nil
		}
		if status != http.StatusNotFound || tpl.id == uuid.Nil {
			return &SendError{Status: status, Code: code}
		}
		warn(s.Logger, eventCallFailed, kindCertificate, status, reasonKeyFallback, "")
		idempotencyKey += ":id"
	}
	status, _, code := s.postTask(ctx, kindCertificate, token, idempotencyKey, withAttachments(template{id: tpl.id}.task(recipientEmail, fullName, vars), attachments))
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status, Code: code}
}

// withAttachments puts attachments into a /mail_tasks/single body; none
// leaves the body as it was.
func withAttachments(task map[string]any, attachments []Attachment) map[string]any {
	if len(attachments) > 0 {
		task["attachments"] = attachments
	}
	return task
}
