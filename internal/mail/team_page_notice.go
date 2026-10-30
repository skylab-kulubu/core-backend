package mail

import (
	"context"
	"strings"
)

// DefaultTeamPageNoticeTemplateKey is the SkyMail template of the team page
// notice (docs/team-page-consent.md, ekip-yetki-devri 21; skymail-frontend
// core-team-page-consent-notice): the one mail a listed member who has not
// decided gets when the transition starts. Variables FirstName (may be
// empty), Teams, HideAt, SettingsURL and Board ("true" or empty).
const DefaultTeamPageNoticeTemplateKey = "core.team-page-consent-notice"

const kindTeamPageNotice = "team_page_notice"

// TeamPageNoticeConfigured reports whether the notice has a template key;
// an empty key turns it off.
func (s *SkyMail) TeamPageNoticeConfigured() bool {
	return s != nil && strings.TrimSpace(s.TeamPageNoticeTemplateKey) != ""
}

// TeamPageNotice posts the notice by template key. A service's /single is
// always SkyMail's islem stream: no preference, no unsubscribe header. It
// reports what SkyMail answered: nil when it took the mail, ErrMaybeDelivered
// when the request went out and no answer came (SkyMail may have taken it;
// /single reads no idempotency key, so the caller must not try again), a
// *SendError when it surely took nothing.
func (s *SkyMail) TeamPageNotice(ctx context.Context, recipient, fullName string, vars map[string]string) error {
	if !s.TeamPageNoticeConfigured() || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	key := strings.TrimSpace(s.TeamPageNoticeTemplateKey)
	status, reached := s.postReached(ctx, kindTeamPageNotice, token, template{key: key}.task(recipient, fullName, vars))
	switch {
	case succeeded(status):
		return nil
	case status == 0 && reached:
		return ErrMaybeDelivered
	}
	return &SendError{Status: status}
}
