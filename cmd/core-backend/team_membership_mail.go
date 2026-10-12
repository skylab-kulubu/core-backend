package main

import (
	"context"
	"slices"

	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
)

// membershipTemplateKeys reads the membership mails' template keys
// (docs/team-membership-mail.md): unset takes the seeded key, empty turns
// that kind off.
func membershipTemplateKeys(sky *mail.SkyMail, lookup func(string) (string, bool)) {
	sky.TeamMembershipTemplateKey = templateKey(lookup, "SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY", mail.DefaultTeamMembershipTemplateKey)
	sky.RoleMembershipTemplateKey = templateKey(lookup, "SKYMAIL_ROLE_MEMBERSHIP_TEMPLATE_KEY", mail.DefaultRoleMembershipTemplateKey)
	sky.GroupMembershipTemplateKey = templateKey(lookup, "SKYMAIL_GROUP_MEMBERSHIP_TEMPLATE_KEY", mail.DefaultGroupMembershipTemplateKey)
}

// startTeamMembershipMail starts the membership mail
// (docs/team-membership-mail.md) until ctx is cancelled: the notifier the
// membership routes tell of each change, its metrics, and a channel that
// closes once its worker has stopped. Without SkyMail, or with all three
// SKYMAIL_*_MEMBERSHIP_TEMPLATE_KEY set empty, it is off: a nil notifier,
// so membership writes read nothing for it, and a closed channel. A kind
// whose key alone is empty is off on its own.
func startTeamMembershipMail(ctx context.Context, queue teammail.Queue, sky *mail.SkyMail, dir identity.Directory, accounts teammail.Accounts, privilegedRoots []string, logf func(string, ...any)) (identity.MembershipNotifier, interface{ Prometheus() string }, <-chan struct{}) {
	off := make(chan struct{})
	close(off)
	if sky == nil {
		logf("team membership mail: off (SkyMail is not configured: KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID and KEYCLOAK_CLIENT_SECRET)")
		return nil, teammail.Off{}, off
	}
	if !slices.ContainsFunc(mail.MembershipKinds, sky.MembershipConfigured) {
		logf("team membership mail: off (SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY, SKYMAIL_ROLE_MEMBERSHIP_TEMPLATE_KEY and SKYMAIL_GROUP_MEMBERSHIP_TEMPLATE_KEY are empty)")
		return nil, teammail.Off{}, off
	}
	svc, err := teammail.NewService(teammail.Config{
		Queue:  queue,
		People: teammail.DirectoryPeople{Directory: dir, Accounts: accounts},
		Groups: dir,
		Mail:   sky,
		// The Privileged roots (AUTHZ_PRIVILEGED_ROOTS) get the role mail.
		PrivilegedRoots: privilegedRoots,
	})
	if err != nil {
		logf("team membership mail: OFF, %v", err)
		return nil, teammail.Off{}, off
	}
	keyOr := func(kind mail.MembershipKind) string {
		if key := sky.MembershipTemplateKey(kind); key != "" {
			return key
		}
		return "off"
	}
	logf("team membership mail: on (template %s; role %s; group %s)", keyOr(mail.MembershipTeam), keyOr(mail.MembershipRole), keyOr(mail.MembershipGroup))
	return svc, svc, svc.Run(ctx, logf)
}
