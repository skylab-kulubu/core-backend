package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// The SkyNotify templates of the mail a person gets when they are added to a
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

// SendError is a send SkyNotify did not take: its status, or 0 when the
// request never reached SkyNotify (no token, transport failure). It names no
// recipient, template or body.
type SendError struct {
	Status int
	// Code is the error code SkyNotify answered a Notification with (its
	// body's code), empty when there is none or it is not code-shaped.
	Code string
}

// codeTypeUnknown is SkyNotify's answer to a Notification whose type is not
// seeded (yet): the same body passes once the Template seed has run.
const codeTypeUnknown = "notification.type_unknown"

func (e *SendError) Error() string {
	if e.Status == 0 {
		return "mail: SkyNotify was not reached"
	}
	return fmt.Sprintf("mail: SkyNotify answered %d", e.Status)
}

// Permanent reports whether SkyNotify refused the request itself (400, 422):
// the same body would be refused again. Everything else — an unknown
// template (404) or Notification type (422 notification.type_unknown), both
// seeded later, a token SkyNotify does not accept (401, 403), a rate limit or
// a failure of its own — can pass on a later try.
func (e *SendError) Permanent() bool {
	if e.Code == codeTypeUnknown {
		return false
	}
	return e.Status == http.StatusBadRequest || e.Status == http.StatusUnprocessableEntity
}

// KeyReused reports whether SkyNotify refused the request because its
// Idempotency-Key was already used for a different body
// (notification.idempotency_key_reused, mail_task.idempotency_key_reused):
// the first request under that key went. Permanent too.
func (e *SendError) KeyReused() bool {
	return strings.HasSuffix(e.Code, "idempotency_key_reused")
}

// MembershipTemplateKey is the template key of kind's mail, empty when the
// kind is off or unknown.
func (s *SkyNotify) MembershipTemplateKey(kind MembershipKind) string {
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
func (s *SkyNotify) MembershipConfigured(kind MembershipKind) bool {
	return s.MembershipTemplateKey(kind) != ""
}

// TeamMembershipConfigured reports whether the team membership mail has a
// template key.
func (s *SkyNotify) TeamMembershipConfigured() bool {
	return s.MembershipConfigured(MembershipTeam)
}

// TeamMembership posts the team membership mail (Membership of
// MembershipTeam).
func (s *SkyNotify) TeamMembership(ctx context.Context, recipient, fullName string, vars map[string]string) error {
	return s.Membership(ctx, MembershipTeam, recipient, fullName, vars)
}

// Membership posts kind's membership mail by template key. Unlike the other
// sends it reports what SkyNotify answered: the mail waits in core's queue
// until SkyNotify has taken it (internal/teammail). A refusal is also warned
// about in the log, like every SkyNotify call, with no recipient; its kind is
// team_membership, role_membership or group_membership.
func (s *SkyNotify) Membership(ctx context.Context, kind MembershipKind, recipient, fullName string, vars map[string]string) error {
	key := s.MembershipTemplateKey(kind)
	if key == "" || s.noBaseURL() {
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
