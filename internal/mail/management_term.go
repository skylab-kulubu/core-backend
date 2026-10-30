package mail

import (
	"context"
	"strings"
)

// DefaultManagementTermTemplateKey is the SkyMail template of the
// management term mails (docs/management-membership-terms.md): the warning
// 14 days and 1 day before a YK, DK or Team Leader membership's term ends,
// the list of terms that expired that day and the weekly summary of those
// still in their Groups. One template; its Notice variable says which.
const DefaultManagementTermTemplateKey = "club.management-term"

const kindManagementTerm = "management_term"

// ManagementTermConfigured reports whether the management term mails have
// a template key; an empty key turns them off.
func (s *SkyMail) ManagementTermConfigured() bool {
	return s != nil && strings.TrimSpace(s.ManagementTermTemplateKey) != ""
}

// ManagementTerm posts a management term mail by template key and reports
// what SkyMail answered, as PrivilegeNotice does: the mail waits in core's
// queue until SkyMail has taken it (internal/membershipterm). A refusal is
// warned about in the log with the kind management_term and no recipient.
func (s *SkyMail) ManagementTerm(ctx context.Context, recipient, fullName string, vars map[string]string) error {
	if !s.ManagementTermConfigured() || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	key := strings.TrimSpace(s.ManagementTermTemplateKey)
	status := s.post(ctx, kindManagementTerm, token, template{key: key}.task(recipient, fullName, vars))
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status}
}
