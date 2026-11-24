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
		status, _, code := s.postTask(ctx, kindCertificate, token, idempotencyKey, template{key: tpl.key}.task(recipientEmail, fullName, vars))
		if succeeded(status) {
			return nil
		}
		if status != http.StatusNotFound || tpl.id == uuid.Nil {
			return &SendError{Status: status, Code: code}
		}
		warn(s.Logger, eventCallFailed, kindCertificate, status, reasonKeyFallback, "")
		idempotencyKey += ":id"
	}
	status, _, code := s.postTask(ctx, kindCertificate, token, idempotencyKey, template{id: tpl.id}.task(recipientEmail, fullName, vars))
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status, Code: code}
}
