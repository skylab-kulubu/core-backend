package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// The SkyMail templates of the mail a person gets when they are added to a
// Group or removed from one (docs/team-membership-mail.md): a team's, a
// Privileged Group's (YK, DK, ADMIN) and any other Group's.
const (
	DefaultTeamMembershipTemplateKey  = "club.team-membership"
	DefaultRoleMembershipTemplateKey  = "club.role-membership"
	DefaultGroupMembershipTemplateKey = "club.group-membership"
)

// MembershipKind is which membership mail a change gets.
type MembershipKind string

const (
	// MembershipTeam: a team, or its LIDERLER/KOORDINATORLER.
	MembershipTeam MembershipKind = "team"
	// MembershipRole: a Privileged root (AUTHZ_PRIVILEGED_ROOTS) or a Group
	// under one.
	MembershipRole MembershipKind = "role"
	// MembershipGroup: every other Group.
	MembershipGroup MembershipKind = "group"
)

// MembershipKinds are the kinds, in the order logs and metrics list them.
var MembershipKinds = []MembershipKind{MembershipTeam, MembershipRole, MembershipGroup}

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

// MembershipTemplateKey is the template key of kind's mail, empty when the
// kind is off or unknown.
func (s *SkyMail) MembershipTemplateKey(kind MembershipKind) string {
	if s == nil {
		return ""
	}
	switch kind {
	case MembershipTeam:
		return strings.TrimSpace(s.TeamMembershipTemplateKey)
	case MembershipRole:
		return strings.TrimSpace(s.RoleMembershipTemplateKey)
	case MembershipGroup:
		return strings.TrimSpace(s.GroupMembershipTemplateKey)
	}
	return ""
}

// MembershipConfigured reports whether kind's mail has a template key.
func (s *SkyMail) MembershipConfigured(kind MembershipKind) bool {
	return s.MembershipTemplateKey(kind) != ""
}

// TeamMembershipConfigured reports whether the team membership mail has a
// template key.
func (s *SkyMail) TeamMembershipConfigured() bool {
	return s.MembershipConfigured(MembershipTeam)
}

// TeamMembership posts the team membership mail (Membership of
// MembershipTeam).
func (s *SkyMail) TeamMembership(ctx context.Context, recipient, fullName string, vars map[string]string) error {
	return s.Membership(ctx, MembershipTeam, recipient, fullName, vars)
}

// Membership posts kind's membership mail by template key. Unlike the other
// sends it reports what SkyMail answered: the mail waits in core's queue
// until SkyMail has taken it (internal/teammail). A refusal is also warned
// about in the log, like every SkyMail call, with no recipient; its kind is
// team_membership, role_membership or group_membership.
func (s *SkyMail) Membership(ctx context.Context, kind MembershipKind, recipient, fullName string, vars map[string]string) error {
	key := s.MembershipTemplateKey(kind)
	if key == "" || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	logKind := kindTeamMembership
	if kind != MembershipTeam {
		logKind = string(kind) + "_membership"
	}
	status := s.post(ctx, logKind, token, template{key: key}.task(recipient, fullName, vars))
	if succeeded(status) {
		return nil
	}
	return &SendError{Status: status}
}
