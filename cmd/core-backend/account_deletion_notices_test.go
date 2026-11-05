package main

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The notices are off until ACCOUNT_DELETION_NOTICES=on: they need SkyMail's
// erase_after_send and their templates seeded first. The keys default to
// the seeded ones, and an empty key turns one notice off.
func TestAccountDeletionNoticesFromEnv(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	sky := &mail.SkyMail{}
	for _, c := range []struct {
		name                string
		env                 map[string]string
		on                  bool
		received, completed string
		fails               bool
	}{
		{name: "unset", env: map[string]string{}},
		{name: "off", env: map[string]string{"ACCOUNT_DELETION_NOTICES": "off"}},
		{name: "on", env: map[string]string{"ACCOUNT_DELETION_NOTICES": "on"}, on: true,
			received: "account.deletion-requested", completed: "account.deletion-completed"},
		{name: "on, received off", env: map[string]string{"ACCOUNT_DELETION_NOTICES": "on", "SKYMAIL_ACCOUNT_DELETION_RECEIVED_TEMPLATE_KEY": ""}, on: true,
			completed: "account.deletion-completed"},
		{name: "on, other key", env: map[string]string{"ACCOUNT_DELETION_NOTICES": "on", "SKYMAIL_ACCOUNT_DELETION_COMPLETED_TEMPLATE_KEY": " account.other "}, on: true,
			received: "account.deletion-requested", completed: "account.other"},
		{name: "true is not on", env: map[string]string{"ACCOUNT_DELETION_NOTICES": "true"}, fails: true},
	} {
		lookup := func(name string) (string, bool) {
			value, ok := c.env[name]
			return value, ok
		}
		notices, err := accountDeletionNotices(lookup, store, sky)
		if c.fails {
			if err == nil || notices != nil {
				t.Errorf("%s: %v, %v; want refused", c.name, notices, err)
			}
			continue
		}
		if err != nil || (notices != nil) != c.on {
			t.Errorf("%s: %v, %v; want on %v", c.name, notices, err, c.on)
			continue
		}
		if notices != nil && (notices.ReceivedTemplateKey != c.received || notices.CompletedTemplateKey != c.completed) {
			t.Errorf("%s: keys %q %q, want %q %q", c.name, notices.ReceivedTemplateKey, notices.CompletedTemplateKey, c.received, c.completed)
		}
	}
}
