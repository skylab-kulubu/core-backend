package mail

import (
	"context"
	"strings"
)

// kindConsentConfirmation is the mail that asks a person to confirm a
// contact consent given for their address (docs/contact-consents.md).
const kindConsentConfirmation = "consent_confirmation"

// ConsentConfirmation sends the confirmation mail of a pending contact
// consent by template key. Like every send it tells its caller nothing: a
// mail SkyMail refuses leaves the consent pending and is warned about in the
// log, never with the address.
func (s *SkyMail) ConsentConfirmation(ctx context.Context, templateKey, recipient, fullName string, vars map[string]string) {
	templateKey = strings.TrimSpace(templateKey)
	if strings.TrimSpace(recipient) == "" || templateKey == "" {
		return
	}
	s.send(ctx, kindConsentConfirmation, template{key: templateKey}, recipient, fullName, vars)
}
