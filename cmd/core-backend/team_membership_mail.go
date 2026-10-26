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
	// The privilege notice (docs/privilege-change-mail.md) the same way.
	sky.PrivilegeNoticeTemplateKey = templateKey(lookup, "SKYMAIL_PRIVILEGE_NOTICE_TEMPLATE_KEY", mail.DefaultPrivilegeNoticeTemplateKey)
	// The Event status mails (docs/event-cancel.md, ticket 14) the same way.
	sky.EventCancelledTemplateKey = templateKey(lookup, "SKYMAIL_EVENT_CANCELLED_TEMPLATE_KEY", mail.DefaultEventCancelledTemplateKey)
	sky.EventReinstatedTemplateKey = templateKey(lookup, "SKYMAIL_EVENT_REINSTATED_TEMPLATE_KEY", mail.DefaultEventReinstatedTemplateKey)
	// The feedback mails (docs/event-feedback.md, ticket 06) the same way.
	sky.FeedbackRequestTemplateKey = templateKey(lookup, "SKYMAIL_FEEDBACK_REQUEST_TEMPLATE_KEY", mail.DefaultFeedbackRequestTemplateKey)
	sky.FeedbackReminderTemplateKey = templateKey(lookup, "SKYMAIL_FEEDBACK_REMINDER_TEMPLATE_KEY", mail.DefaultFeedbackReminderTemplateKey)
}

// startTeamMembershipMail starts the membership mail
// (docs/team-membership-mail.md) and the privilege notice
// (docs/privilege-change-mail.md) until ctx is cancelled: the notifiers the
// membership and role mapping routes tell of each change, the metrics, and
// a channel that closes once the worker has stopped. Without SkyMail, or
// with all three SKYMAIL_*_MEMBERSHIP_TEMPLATE_KEY and
// SKYMAIL_PRIVILEGE_NOTICE_TEMPLATE_KEY set empty, it is off: nil
// notifiers, so the writes read nothing for them, and a closed channel. A
// kind whose key alone is empty is off on its own; so is the notice
// without a notice queue.
func startTeamMembershipMail(ctx context.Context, queue teammail.Queue, notices teammail.NoticeQueue, sky *mail.SkyMail, dir identity.Directory, accounts teammail.Accounts, privilegedRoots []string, logf func(string, ...any)) (identity.MembershipNotifier, identity.GroupRolesNotifier, interface{ Prometheus() string }, <-chan struct{}) {
	off := make(chan struct{})
	close(off)
	if sky == nil {
		logf("team membership mail: off (SkyMail is not configured: KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID and KEYCLOAK_CLIENT_SECRET)")
		return nil, nil, teammail.Off{}, off
	}
	noticeOn := notices != nil && sky.PrivilegeNoticeConfigured()
	if !slices.ContainsFunc(mail.MembershipKinds, sky.MembershipConfigured) && !noticeOn {
		logf("team membership mail: off (SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY, SKYMAIL_ROLE_MEMBERSHIP_TEMPLATE_KEY, SKYMAIL_GROUP_MEMBERSHIP_TEMPLATE_KEY and SKYMAIL_PRIVILEGE_NOTICE_TEMPLATE_KEY are empty)")
		return nil, nil, teammail.Off{}, off
	}
	config := teammail.Config{
		Queue:  queue,
		People: teammail.DirectoryPeople{Directory: dir, Accounts: accounts},
		Groups: dir,
		Mail:   sky,
		// The Privileged roots (AUTHZ_PRIVILEGED_ROOTS) get the role mail,
		// and the privilege notice tells the trees of those named ADMIN.
		PrivilegedRoots: privilegedRoots,
	}
	if notices != nil {
		config.Notice = &teammail.NoticeConfig{Queue: notices, Rosters: teammail.DirectoryRosters{Directory: dir}, Mail: sky}
	}
	svc, err := teammail.NewService(config)
	if err != nil {
		logf("team membership mail: OFF, %v", err)
		return nil, nil, teammail.Off{}, off
	}
	keyOr := func(kind mail.MembershipKind) string {
		if key := sky.MembershipTemplateKey(kind); key != "" {
			return key
		}
		return "off"
	}
	notice := "off"
	if noticeOn {
		notice = sky.PrivilegeNoticeTemplateKey
	}
	logf("team membership mail: on (template %s; role %s; group %s; privilege notice %s)", keyOr(mail.MembershipTeam), keyOr(mail.MembershipRole), keyOr(mail.MembershipGroup), notice)
	var groupRoles identity.GroupRolesNotifier
	if noticeOn {
		groupRoles = svc
	}
	return svc, groupRoles, svc, svc.Run(ctx, logf)
}
