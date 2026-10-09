package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
		{name: "on", sky: &mail.SkyMail{BaseURL: "http://127.0.0.1:1", TeamMembershipTemplateKey: mail.DefaultTeamMembershipTemplateKey, Tokens: mail.StaticToken("t")},
			on: true, logged: "team membership mail: on (template club.team-membership)"},
		{name: "no SkyMail", logged: "team membership mail: off (SkyMail is not configured"},
		{name: "empty key", sky: &mail.SkyMail{BaseURL: "http://127.0.0.1:1", Tokens: mail.StaticToken("t")},
			logged: "team membership mail: off (SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY is empty)"},
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
			notifier, metrics, stopped := startTeamMembershipMail(ctx, teammail.NewMemoryQueue(), test.sky, identity.NewMemory(), user.NewMemoryStore(), logf)
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
