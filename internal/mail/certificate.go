package mail

import (
	"context"
	"strings"
)

func (s *SkyMail) Certificate(ctx context.Context, recipientEmail, fullName string, vars map[string]string) {
	if strings.TrimSpace(recipientEmail) == "" {
		return
	}
	s.send(ctx, kindCertificate, s.certificateTemplate(), recipientEmail, fullName, vars)
}
