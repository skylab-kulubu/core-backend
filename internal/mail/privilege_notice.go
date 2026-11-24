package mail

import (
	"context"
	"strings"
)

// DefaultPrivilegeNoticeTemplateKey is the SkyNotify template of the privilege
// notice (docs/privilege-change-mail.md): the mail ADMIN, and on a
// Privileged Group's membership the root's own members, get when management
// rights are given or taken.
const DefaultPrivilegeNoticeTemplateKey = "club.privilege-change"

const kindPrivilegeNotice = "privilege_notice"

// PrivilegeNoticeConfigured reports whether the privilege notice has a
// template key; an empty key turns it off.
func (s *SkyNotify) PrivilegeNoticeConfigured() bool {
	return s != nil && strings.TrimSpace(s.PrivilegeNoticeTemplateKey) != ""
}

// PrivilegeNotice posts the privilege notice by template key and reports
// what SkyNotify answered, as Membership does: the notice waits in core's
// queue until SkyNotify has taken it (internal/teammail). A refusal is warned
// about in the log with the kind privilege_notice and no recipient.
func (s *SkyNotify) PrivilegeNotice(ctx context.Context, recipient, fullName string, vars map[string]string) error {
	if !s.PrivilegeNoticeConfigured() || s.noBaseURL() {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	key := strings.TrimSpace(s.PrivilegeNoticeTemplateKey)
	status := s.post(ctx, kindPrivilegeNotice, token, template{key: key}.task(recipient, fullName, vars))
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status}
}
