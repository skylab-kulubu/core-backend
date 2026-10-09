package main

import (
	"context"

	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
)

// startTeamMembershipMail starts the team membership mail
// (docs/team-membership-mail.md) until ctx is cancelled: the notifier the
// membership routes tell of each change, its metrics, and a channel that
// closes once its worker has stopped. Without SkyMail or with
// SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY set empty it is off: a nil notifier,
// so membership writes read nothing for it, and a closed channel.
func startTeamMembershipMail(ctx context.Context, queue teammail.Queue, sky *mail.SkyMail, dir identity.Directory, accounts teammail.Accounts, logf func(string, ...any)) (identity.MembershipNotifier, interface{ Prometheus() string }, <-chan struct{}) {
	off := make(chan struct{})
	close(off)
	switch {
	case sky == nil:
		logf("team membership mail: off (SkyMail is not configured: KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID and KEYCLOAK_CLIENT_SECRET)")
		return nil, teammail.Off{}, off
	case !sky.TeamMembershipConfigured():
		logf("team membership mail: off (SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY is empty)")
		return nil, teammail.Off{}, off
	}
	svc, err := teammail.NewService(teammail.Config{
		Queue:  queue,
		People: teammail.DirectoryPeople{Directory: dir, Accounts: accounts},
		Groups: dir,
		Mail:   sky,
	})
	if err != nil {
		logf("team membership mail: OFF, %v", err)
		return nil, teammail.Off{}, off
	}
	logf("team membership mail: on (template %s)", sky.TeamMembershipTemplateKey)
	return svc, svc, svc.Run(ctx, logf)
}
