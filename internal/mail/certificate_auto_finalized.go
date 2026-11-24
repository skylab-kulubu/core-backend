package mail

import (
	"context"
	"strings"
)

// DefaultCertificateAutoFinalizedTemplateKey is the SkyMail template that
// tells an Event's publisher its attendance closed by itself
// (docs/certificate-rule.md, S6). Variables FirstName, EventName, EventId,
// CertificateCount (the certificates queued) and OwnerTeam (the organizer
// as the club shows it).
const DefaultCertificateAutoFinalizedTemplateKey = "core.certificate-auto-finalized"

const kindCertificateAutoFinalized = "certificate_auto_finalized"

// CertificateAutoFinalized posts the mail by template key, as the
// certificate mail goes: nothing is told to the caller, a refusal is warned
// about in the log. An empty key (SKYMAIL_CERTIFICATE_AUTO_FINALIZED_TEMPLATE_KEY="")
// turns it off.
func (s *SkyMail) CertificateAutoFinalized(ctx context.Context, recipientEmail, fullName string, vars map[string]string) {
	if s == nil || strings.TrimSpace(recipientEmail) == "" {
		return
	}
	s.send(ctx, kindCertificateAutoFinalized, template{key: strings.TrimSpace(s.CertificateAutoFinalizedTemplateKey)}, recipientEmail, fullName, vars)
}
