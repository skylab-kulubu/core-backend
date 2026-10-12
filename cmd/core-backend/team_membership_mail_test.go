package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The team membership mail is on only with SkyMail and a template key; off,
// nothing hears of membership changes and /v1/metrics says so.
func TestStartTeamMembershipMail(t *testing.T) {
	for _, test := range []struct {
		name   string
		sky    *mail.SkyMail
		on     bool
		logged string
	}{
		{name: "on", sky: &mail.SkyMail{
			BaseURL: "http://127.0.0.1:1", TeamMembershipTemplateKey: mail.DefaultTeamMembershipTemplateKey,
			RoleMembershipTemplateKey: mail.DefaultRoleMembershipTemplateKey, GroupMembershipTemplateKey: mail.DefaultGroupMembershipTemplateKey,
			Tokens: mail.StaticToken("t"),
		},
			on: true, logged: "team membership mail: on (template club.team-membership; role club.role-membership; group club.group-membership)"},
		{name: "team only", sky: &mail.SkyMail{BaseURL: "http://127.0.0.1:1", TeamMembershipTemplateKey: mail.DefaultTeamMembershipTemplateKey, Tokens: mail.StaticToken("t")},
			on: true, logged: "team membership mail: on (template club.team-membership; role off; group off)"},
		{name: "teams off", sky: &mail.SkyMail{BaseURL: "http://127.0.0.1:1", GroupMembershipTemplateKey: mail.DefaultGroupMembershipTemplateKey, Tokens: mail.StaticToken("t")},
			on: true, logged: "team membership mail: on (template off; role off; group club.group-membership)"},
		{name: "no SkyMail", logged: "team membership mail: off (SkyMail is not configured"},
		{name: "empty keys", sky: &mail.SkyMail{BaseURL: "http://127.0.0.1:1", Tokens: mail.StaticToken("t")},
			logged: "team membership mail: off (SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY, SKYMAIL_ROLE_MEMBERSHIP_TEMPLATE_KEY and SKYMAIL_GROUP_MEMBERSHIP_TEMPLATE_KEY are empty)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var logs strings.Builder
			logf := func(format string, a ...any) {
				mu.Lock()
				defer mu.Unlock()
				fmt.Fprintf(&logs, format+"\n", a...)
			}
			ctx, cancel := context.WithCancel(context.Background())
			notifier, metrics, stopped := startTeamMembershipMail(ctx, teammail.NewMemoryQueue(), test.sky, identity.NewMemory(), user.NewMemoryStore(), nil, logf)
			cancel()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("the worker did not stop after cancel")
			}
			mu.Lock()
			defer mu.Unlock()
			if (notifier != nil) != test.on || !strings.Contains(logs.String(), test.logged) {
				t.Fatalf("notifier %v, log:\n%s", notifier, logs.String())
			}
			want := "skylab_team_membership_mail_enabled 0"
			if test.on {
				want = "skylab_team_membership_mail_enabled 1"
			}
			if !strings.Contains(metrics.Prometheus(), want) {
				t.Fatalf("metrics:\n%s", metrics.Prometheus())
			}
		})
	}
}

func TestTeamMembershipTemplateKeyFromEnv(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			v, ok := values[name]
			return v, ok
		}
	}
	if got := templateKey(lookup(nil), "SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY", mail.DefaultTeamMembershipTemplateKey); got != "club.team-membership" {
		t.Fatalf("unset = %q", got)
	}
	if got := templateKey(lookup(map[string]string{"SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY": ""}), "SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY", mail.DefaultTeamMembershipTemplateKey); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

// The role and group mails follow the team mail's variables: unset takes
// the seeded key, empty turns the kind off.
func TestMembershipTemplateKeysFromEnv(t *testing.T) {
	env := func(values map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			v, ok := values[name]
			return v, ok
		}
	}
	sky := &mail.SkyMail{}
	membershipTemplateKeys(sky, env(nil))
	if sky.TeamMembershipTemplateKey != "club.team-membership" || sky.RoleMembershipTemplateKey != "club.role-membership" || sky.GroupMembershipTemplateKey != "club.group-membership" {
		t.Fatalf("unset = %+v", sky)
	}
	sky = &mail.SkyMail{}
	membershipTemplateKeys(sky, env(map[string]string{"SKYMAIL_ROLE_MEMBERSHIP_TEMPLATE_KEY": "", "SKYMAIL_GROUP_MEMBERSHIP_TEMPLATE_KEY": " club.custom "}))
	if sky.TeamMembershipTemplateKey != "club.team-membership" || sky.RoleMembershipTemplateKey != "" || sky.GroupMembershipTemplateKey != "club.custom" {
		t.Fatalf("set = %+v", sky)
	}
}

// The membership routes' service, wired as main wires it, queues a mail for
// every Group (Yusuf, 2026-10-12): the Privileged Groups and the areas too,
// each with its kind; an add and a removal before the mail went cancel out.
func TestMembershipMailQueuesEveryGroupThroughTheService(t *testing.T) {
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
		{ID: "g-dk", Name: "DK", Path: "/UYELER/DK"},
	} {
		dir.PutGroup(g)
	}
	person := uuid.New()
	dir.PutUser(identity.Person{ID: person, Email: "ada@example.test", FirstName: "Ada", Enabled: true})
	sky := &mail.SkyMail{BaseURL: "http://127.0.0.1:1", Tokens: mail.StaticToken("t")}
	membershipTemplateKeys(sky, func(string) (string, bool) { return "", false })
	queue := teammail.NewMemoryQueue()
	ctx, cancel := context.WithCancel(context.Background())
	notifier, _, stopped := startTeamMembershipMail(ctx, queue, sky, dir, user.NewMemoryStore(), []string{"/ADMIN", "/UYELER/YK", "/UYELER/DK"}, func(string, ...any) {})
	// The worker stops first, so the queue keeps what the routes wrote.
	cancel()
	<-stopped
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{Membership: notifier})
	privileged := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}

	for _, group := range []string{"g-dk", "g-arge", "g-weblab", "g-yk"} {
		if err := svc.AddMember(ctx, privileged, group, person); err != nil {
			t.Fatalf("add %s: %v", group, err)
		}
	}
	if err := svc.RemoveMember(ctx, privileged, "g-yk", person); err != nil {
		t.Fatal(err)
	}
	got := map[string]teammail.Kind{}
	for _, c := range queue.Snapshot() {
		got[c.GroupPath] = c.Kind
	}
	want := map[string]teammail.Kind{"/UYELER/DK": teammail.KindRole, "/UYELER/ARGE": teammail.KindGroup, "/UYELER/ARGE/WEBLAB": teammail.KindTeam}
	if len(got) != len(want) {
		t.Fatalf("queued %v", got)
	}
	for path, kind := range want {
		if got[path] != kind {
			t.Fatalf("queued %v, want %v", got, want)
		}
	}
}
