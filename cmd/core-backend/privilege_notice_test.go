package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The acceptance tests of grup-yonetimi 05, wired as main wires it: the
// identity routes' service, the membership mail and the privilege notice on
// one worker, SkyMail answering 201.
func TestPrivilegeNoticeThroughTheService(t *testing.T) {
	var mu sync.Mutex
	sent := map[string][]string{} // template key → recipients
	skymail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key       string `json:"template_key"`
			Recipient string `json:"recipient_email"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		sent[body.Key] = append(sent[body.Key], body.Recipient)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(skymail.Close)

	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-admin", Name: "ADMIN", Path: "/ADMIN"},
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
	} {
		dir.PutGroup(g)
	}
	ctx := context.Background()
	person := func(name string, groups ...string) uuid.UUID {
		id := uuid.New()
		dir.PutUser(identity.Person{ID: id, Email: name + "@example.test", FirstName: name, Enabled: true})
		for _, g := range groups {
			if err := dir.AddMember(ctx, g, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	person("root", "g-admin")
	actorID := person("baskan", "g-yk", "g-uyeler")
	person("sayman", "g-yk", "g-uyeler")
	for i := range 5 {
		person("uye"+string(rune('a'+i)), "g-uyeler")
	}
	ada := person("ada", "g-uyeler")

	sky := &mail.SkyMail{BaseURL: skymail.URL, Tokens: mail.StaticToken("t"), HTTP: skymail.Client()}
	membershipTemplateKeys(sky, func(string) (string, bool) { return "", false })
	if sky.PrivilegeNoticeTemplateKey != "club.privilege-change" {
		t.Fatalf("default key %q", sky.PrivilegeNoticeTemplateKey)
	}
	queue, notices := teammail.NewMemoryQueue(), teammail.NewMemoryNoticeQueue()
	workers, stop := context.WithCancel(ctx)
	notifier, groupRoles, _, stopped := startTeamMembershipMail(workers, queue, notices, sky, dir, user.NewMemoryStore(), []string{"/ADMIN", "/UYELER/YK", "/UYELER/DK"}, func(string, ...any) {})
	stop()
	<-stopped
	if groupRoles == nil {
		t.Fatal("the role mapping notifier is off")
	}
	worker := notifier.(*teammail.Service)
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		Membership: notifier, GroupRoles: groupRoles,
	})
	actor := authz.Principal{ID: actorID.String(), Groups: []string{"/UYELER/YK"}}
	drain := func() map[string][]string {
		t.Helper()
		if _, err := worker.Pass(ctx); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		out := map[string][]string{}
		for k, v := range sent {
			out[k] = slices.Sorted(slices.Values(v))
		}
		clear(sent)
		return out
	}

	// YK gets ada: ADMIN and YK's other member are told, the actor is not,
	// ada gets her own role membership mail.
	if err := svc.AddMember(ctx, actor, "g-yk", ada); err != nil {
		t.Fatal(err)
	}
	got := drain()
	if !slices.Equal(got["club.privilege-change"], []string{"root@example.test", "sayman@example.test"}) ||
		!slices.Equal(got["club.role-membership"], []string{"ada@example.test"}) {
		t.Fatalf("YK add sent %v", got)
	}

	// A role mapped to /UYELER: ADMIN alone, none of the club's members.
	if err := svc.SetGroupClientRoles(ctx, actor, "g-uyeler", []identity.ClientRole{{ClientID: "forms", Role: "viewer"}}); err != nil {
		t.Fatal(err)
	}
	if got := drain(); len(got) != 1 || !slices.Equal(got["club.privilege-change"], []string{"root@example.test"}) {
		t.Fatalf("role mapping sent %v", got)
	}
	// The same mapping again changes nothing and sends nothing.
	if err := svc.SetGroupClientRoles(ctx, actor, "g-uyeler", []identity.ClientRole{{ClientID: "forms", Role: "viewer"}}); err != nil {
		t.Fatal(err)
	}
	if got := drain(); len(got) != 0 {
		t.Fatalf("unchanged mapping sent %v", got)
	}

	// An ordinary team: the person's team mail only, no notice.
	if err := svc.AddMember(ctx, actor, "g-weblab", ada); err != nil {
		t.Fatal(err)
	}
	if got := drain(); len(got) != 1 || !slices.Equal(got["club.team-membership"], []string{"ada@example.test"}) {
		t.Fatalf("team add sent %v", got)
	}

	// Removed from YK: told the same way.
	if err := svc.RemoveMember(ctx, actor, "g-yk", ada); err != nil {
		t.Fatal(err)
	}
	got = drain()
	if !slices.Equal(got["club.privilege-change"], []string{"root@example.test", "sayman@example.test"}) ||
		!slices.Equal(got["club.role-membership"], []string{"ada@example.test"}) {
		t.Fatalf("YK removal sent %v", got)
	}
}

// SKYMAIL_PRIVILEGE_NOTICE_TEMPLATE_KEY alone keeps the worker on; set
// empty, the role mapping notifier is off.
func TestPrivilegeNoticeKeyFromEnv(t *testing.T) {
	for _, test := range []struct {
		name string
		env  map[string]string
		on   bool
	}{
		{"notice only", map[string]string{
			"SKYMAIL_TEAM_MEMBERSHIP_TEMPLATE_KEY": "", "SKYMAIL_ROLE_MEMBERSHIP_TEMPLATE_KEY": "", "SKYMAIL_GROUP_MEMBERSHIP_TEMPLATE_KEY": "",
		}, true},
		{"notice off", map[string]string{"SKYMAIL_PRIVILEGE_NOTICE_TEMPLATE_KEY": ""}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			sky := &mail.SkyMail{BaseURL: "http://127.0.0.1:1", Tokens: mail.StaticToken("t")}
			membershipTemplateKeys(sky, func(name string) (string, bool) { v, ok := test.env[name]; return v, ok })
			ctx, cancel := context.WithCancel(context.Background())
			membership, groupRoles, _, stopped := startTeamMembershipMail(ctx, teammail.NewMemoryQueue(), teammail.NewMemoryNoticeQueue(), sky, identity.NewMemory(), user.NewMemoryStore(), nil, func(string, ...any) {})
			cancel()
			<-stopped
			if (groupRoles != nil) != test.on || membership == nil {
				t.Fatalf("membership %v groupRoles %v", membership, groupRoles)
			}
		})
	}
}
