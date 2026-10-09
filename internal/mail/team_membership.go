package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// DefaultTeamMembershipTemplateKey is the SkyMail template of the mail a
// person gets when they are added to a team or removed from it
// (docs/team-membership-mail.md).
const DefaultTeamMembershipTemplateKey = "club.team-membership"

const kindTeamMembership = "team_membership"

// ErrTemplateUnconfigured is a send whose mail kind has no template to
// address.
var ErrTemplateUnconfigured = errors.New("mail: template unconfigured")

// SendError is a send SkyMail did not take: its status, or 0 when the
// request never reached SkyMail (no token, transport failure). It names no
// recipient, template or body.
type SendError struct {
	Status int
}

func (e *SendError) Error() string {
	if e.Status == 0 {
		return "mail: SkyMail was not reached"
	}
	return fmt.Sprintf("mail: SkyMail answered %d", e.Status)
}

// Permanent reports whether SkyMail refused the request itself (400, 422):
// the same body would be refused again. Everything else — an unknown
// template (404, it may be seeded later), a token SkyMail does not accept
// (401, 403), a rate limit or a failure of its own — can pass on a later try.
func (e *SendError) Permanent() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusUnprocessableEntity
}

// TeamMembershipConfigured reports whether the team membership mail has a
// template key.
func (s *SkyMail) TeamMembershipConfigured() bool {
	return s != nil && strings.TrimSpace(s.TeamMembershipTemplateKey) != ""
}

// TeamMembership posts the team membership mail by template key. Unlike the
// other sends it reports what SkyMail answered: the mail waits in core's
// queue until SkyMail has taken it (internal/teammail). A refusal is also
// warned about in the log, like every SkyMail call, with no recipient.
func (s *SkyMail) TeamMembership(ctx context.Context, recipient, fullName string, vars map[string]string) error {
	if !s.TeamMembershipConfigured() || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	status := s.post(ctx, kindTeamMembership, token, template{key: strings.TrimSpace(s.TeamMembershipTemplateKey)}.task(recipient, fullName, vars))
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status}
}
