package teammail

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

type noticeSent struct {
	recipient string
	vars      map[string]string
}

type fakeNoticeMail struct {
	off  bool
	err  error
	sent []noticeSent
}

func (m *fakeNoticeMail) PrivilegeNoticeConfigured() bool { return !m.off }

func (m *fakeNoticeMail) PrivilegeNotice(_ context.Context, recipient, _ string, vars map[string]string) error {
	if m.off {
		return mail.ErrTemplateUnconfigured
	}
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, noticeSent{recipient, vars})
	return nil
}

// fakeRosters are the people in each Group's tree, by the Group's path.
type fakeRosters struct {
	trees map[string][]uuid.UUID
	err   error
	// failing are paths whose tree cannot be read.
	failing map[string]bool
	reads   []string
}

func (r *fakeRosters) MemberIDs(_ context.Context, path string) ([]uuid.UUID, error) {
	r.reads = append(r.reads, path)
	if r.err != nil || r.failing[path] {
		return nil, errors.New("keycloak timed out")
	}
	return r.trees[path], nil
}

// noticeFixture: ADMIN holds root and fatih; YK holds fatih (also ADMIN),
// can and the actor; the club (/UYELER) has 300 members besides.
type noticeFixture struct {
	svc                   *Service
	queue                 *MemoryNoticeQueue
	members               *MemoryQueue
	mail                  *fakeNoticeMail
	membershipMail        *fakeMail
	rosters               *fakeRosters
	people                *fakePeople
	clock                 *clock
	logs                  *bytes.Buffer
	root, fatih, can, act uuid.UUID
	ada, svcAccount       uuid.UUID
	club                  []uuid.UUID
}

func newNoticeFixture(t *testing.T) *noticeFixture {
	t.Helper()
	f := &noticeFixture{
		queue: NewMemoryNoticeQueue(), members: NewMemoryQueue(), mail: &fakeNoticeMail{}, membershipMail: &fakeMail{},
		clock: &clock{now: time.Date(2026, 10, 12, 19, 15, 0, 0, time.UTC)}, logs: &bytes.Buffer{},
		root: uuid.New(), fatih: uuid.New(), can: uuid.New(), act: uuid.New(), ada: uuid.New(), svcAccount: uuid.New(),
	}
	for range 300 {
		f.club = append(f.club, uuid.New())
	}
	f.rosters = &fakeRosters{trees: map[string][]uuid.UUID{
		"/ADMIN":     {f.root, f.fatih, f.svcAccount},
		"/UYELER/YK": {f.fatih, f.can, f.act, f.ada},
		"/UYELER":    append(slices.Clone(f.club), f.can, f.ada),
	}}
	f.people = &fakePeople{
		recipients: map[uuid.UUID]Recipient{
			f.root: {Email: "root@example.test", FullName: "Root"}, f.fatih: {Email: "fatih@example.test", FullName: "Fatih Naz"},
			f.can: {Email: "can@example.test", FullName: "Can"}, f.ada: {Email: "ada@example.test", FullName: "Ada Lovelace"},
			f.act: {Email: "act@example.test", FullName: "Aktör"},
		},
		skip:  map[uuid.UUID]SkipReason{f.svcAccount: SkipNoEmail},
		names: map[uuid.UUID]string{f.act: "Aktör Kişi", f.ada: "Ada Lovelace"},
	}
	for _, id := range f.club {
		f.people.recipients[id] = Recipient{Email: id.String() + "@example.test"}
	}
	svc, err := NewService(Config{
		Queue: f.members, People: f.people, Mail: f.membershipMail, Now: f.clock.Now, Logger: log.New(f.logs, "", 0),
		Groups: fakeGroups{
			"/UYELER/YK": {ID: "g-yk", Name: "YK", Path: "/UYELER/YK", Attributes: map[string]string{"display_name_tr": "Yönetim Kurulu"}},
			"/UYELER":    {ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		},
		PrivilegedRoots: []string{"/ADMIN", "/UYELER/YK", "/UYELER/DK"},
		Notice:          &NoticeConfig{Queue: f.queue, Rosters: f.rosters, Mail: f.mail},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func (f *noticeFixture) recipients() []string {
	var out []string
	for _, s := range f.mail.sent {
		out = append(out, s.recipient)
	}
	slices.Sort(out)
	return out
}

func (f *noticeFixture) pass(t *testing.T) Report {
	t.Helper()
	report, err := f.svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// Rule 2: a person added to a Privileged Group. ADMIN and the root's
// members are told, once each; the actor is not; the person added gets
// their own role membership mail instead; a service account (no e-mail)
// is skipped.
func TestNoticeOfAPrivilegedAddGoesToAdminAndTheRoot(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.svc.MembershipChanged(context.Background(), identity.MembershipChange{
		UserID: f.ada, Group: identity.Group{ID: "g-yk", Path: "/UYELER/YK"}, Added: true, ActorID: f.act.String(),
	})
	report := f.pass(t)
	want := []string{"can@example.test", "fatih@example.test", "root@example.test"}
	if got := f.recipients(); !slices.Equal(got, want) {
		t.Fatalf("notice recipients %v, want %v", got, want)
	}
	if report.Notices.Read != 1 || report.Notices.Sent != 3 || report.Notices.Skipped != 1 {
		t.Fatalf("report %+v", report.Notices)
	}
	// The person added hears of it through the role membership mail.
	if len(f.membershipMail.sent) != 1 || f.membershipMail.sent[0].recipient != "ada@example.test" || f.membershipMail.sent[0].kind != KindRole {
		t.Fatalf("membership mail %+v", f.membershipMail.sent)
	}
	vars := f.mail.sent[0].vars
	wantVars := map[string]string{
		"Change": "member-added", "GroupName": "YK · Yönetim Kurulu", "GroupId": "g-yk", "Root": "board",
		"SubjectName": "Ada Lovelace", "ActorName": "Aktör Kişi", "OccurredAt": "12.10.2026 22:15",
		"RolesAdded": "", "RolesRemoved": "", "AdminRole": "no", "MemberCount": "",
	}
	if len(vars) != len(wantVars) {
		t.Fatalf("vars %v", vars)
	}
	for k, v := range wantVars {
		if vars[k] != v {
			t.Fatalf("vars[%s] = %q, want %q (%v)", k, vars[k], v, vars)
		}
	}
	if len(f.queue.Snapshot()) != 0 {
		t.Fatalf("left %+v", f.queue.Snapshot())
	}
}

func TestNoticeOfAPrivilegedRemoval(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.svc.MembershipChanged(context.Background(), identity.MembershipChange{
		UserID: f.can, Group: identity.Group{Path: "/UYELER/YK/BASKAN"}, Added: false, ActorID: f.act.String(),
	})
	f.pass(t)
	// The root's tree, not only the subgroup: fatih and ada sit in YK.
	want := []string{"ada@example.test", "fatih@example.test", "root@example.test"}
	if got := f.recipients(); !slices.Equal(got, want) {
		t.Fatalf("recipients %v, want %v", got, want)
	}
	if v := f.mail.sent[0].vars; v["Change"] != "member-removed" || v["Root"] != "board" || v["GroupName"] != "BASKAN" {
		t.Fatalf("vars %v", v)
	}
}

// Rule 1: a role mapped to /UYELER goes to ADMIN alone; none of the 300
// members hears of it. The mail counts them and says whether a management
// role changed.
func TestNoticeOfARoleMappingGoesToAdminOnly(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.svc.GroupRolesChanged(context.Background(), identity.GroupRolesChange{
		Group:   identity.Group{ID: "g-uyeler", Path: "/UYELER"},
		Added:   []identity.ClientRole{{ClientID: "forms", Role: "viewer"}, {ClientID: "core", Role: "users:manage"}},
		Removed: []identity.ClientRole{{ClientID: "site", Role: "cms:access"}},
		ActorID: f.fatih.String(),
	})
	f.pass(t)
	// fatih made the change: only the other ADMIN member is told.
	if got := f.recipients(); !slices.Equal(got, []string{"root@example.test"}) {
		t.Fatalf("recipients %v", got)
	}
	v := f.mail.sent[0].vars
	if v["Change"] != "roles-changed" || v["RolesAdded"] != "core:users:manage, forms:viewer" || v["RolesRemoved"] != "site:cms:access" ||
		v["AdminRole"] != "yes" || v["MemberCount"] != "302" || v["Root"] != "" || v["SubjectName"] != "" || v["GroupName"] != "UYELER" {
		t.Fatalf("vars %v", v)
	}
	// The Group's tree is read to count it, never to mail it.
	if slices.Contains(f.recipients(), f.club[0].String()+"@example.test") {
		t.Fatal("a club member was told")
	}
}

// AdminRole follows authz's role classes (#240): a Site's cms:access and a
// realm role are admin roles; an app role and an unclassified one are not.
func TestNoticeAdminRoleFollowsTheRoleClasses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		role  identity.ClientRole
		want  string
		shown string
	}{
		{identity.ClientRole{ClientID: "frontend-main", Role: "cms:access"}, "yes", "frontend-main:cms:access"},
		{identity.ClientRole{ClientID: "", Role: "ADMIN"}, "yes", "realm:ADMIN"},
		{identity.ClientRole{ClientID: "core", Role: "users:manage"}, "yes", "core:users:manage"},
		{identity.ClientRole{ClientID: "forms", Role: "viewer"}, "no", "forms:viewer"},
	} {
		f := newNoticeFixture(t)
		f.svc.GroupRolesChanged(context.Background(), identity.GroupRolesChange{
			Group: identity.Group{Path: "/UYELER"}, Removed: []identity.ClientRole{tc.role}, ActorID: f.act.String(),
		})
		f.pass(t)
		if len(f.mail.sent) == 0 {
			t.Fatalf("%v: nothing sent", tc.role)
		}
		if v := f.mail.sent[0].vars; v["AdminRole"] != tc.want || v["RolesRemoved"] != tc.shown {
			t.Fatalf("%v: AdminRole %q RolesRemoved %q", tc.role, v["AdminRole"], v["RolesRemoved"])
		}
	}
}

func TestNoticeOfAnAppRoleSaysNoAdminRole(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.svc.GroupRolesChanged(context.Background(), identity.GroupRolesChange{
		Group: identity.Group{Path: "/UYELER"}, Added: []identity.ClientRole{{ClientID: "forms", Role: "viewer"}}, ActorID: f.act.String(),
	})
	f.pass(t)
	if len(f.mail.sent) != 2 || f.mail.sent[0].vars["AdminRole"] != "no" {
		t.Fatalf("sent %+v", f.mail.sent)
	}
}

// A team, an area and the club are no Privileged Group: their membership
// sends no notice (the person's own mail still goes).
func TestNoNoticeForOrdinaryGroups(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	for _, path := range []string{"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE", "/UYELER", "/UYELER/ARGE/WEBLAB/ADMIN"} {
		f.svc.MembershipChanged(context.Background(), identity.MembershipChange{UserID: f.ada, Group: identity.Group{Path: path}, Added: true, ActorID: f.act.String()})
	}
	if n := len(f.queue.Snapshot()); n != 0 {
		t.Fatalf("%d notices queued", n)
	}
	if len(f.members.Snapshot()) != 4 {
		t.Fatalf("membership mails %+v", f.members.Snapshot())
	}
	// A write that changed no role tells nothing.
	f.svc.GroupRolesChanged(context.Background(), identity.GroupRolesChange{Group: identity.Group{Path: "/UYELER"}, ActorID: f.act.String()})
	if n := len(f.queue.Snapshot()); n != 0 {
		t.Fatalf("%d notices queued for no change", n)
	}
}

// An add and a removal before any mail went are both told: a right held
// for a moment was held (unlike the membership mail, nothing cancels).
func TestNoticesDoNotCancelOut(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	for _, added := range []bool{true, false} {
		f.svc.MembershipChanged(context.Background(), identity.MembershipChange{UserID: f.ada, Group: identity.Group{Path: "/ADMIN"}, Added: added, ActorID: f.act.String()})
	}
	f.pass(t)
	if len(f.mail.sent) != 4 { // root and fatih, twice; ADMIN's root is the change's root too
		t.Fatalf("sent %d: %+v", len(f.mail.sent), f.recipients())
	}
}

// Not seeded yet (SkyMail 404): each notice waits and is retried, as the
// membership mail does, and the metric says so.
func TestNoticeWaitsForAnUnseededTemplate(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.mail.err = &mail.SendError{Status: http.StatusNotFound}
	f.svc.GroupRolesChanged(context.Background(), identity.GroupRolesChange{
		Group: identity.Group{Path: "/UYELER/YK"}, Added: []identity.ClientRole{{ClientID: "core", Role: "audit:read"}}, ActorID: f.act.String(),
	})
	report := f.pass(t)
	if report.Notices.Failed != 2 || len(f.queue.Snapshot()) != 2 {
		t.Fatalf("report %+v, queue %+v", report.Notices, f.queue.Snapshot())
	}
	for _, n := range f.queue.Snapshot() {
		if n.RecipientID == nil || n.Attempts != 1 || !n.NextAttemptAt.Equal(f.clock.now.Add(RetryFirst)) {
			t.Fatalf("waiting %+v", n)
		}
	}
	if !strings.Contains(f.svc.Prometheus(), `skylab_privilege_notice_mail_total{outcome="failed"} 2`) {
		t.Fatalf("metrics:\n%s", f.svc.Prometheus())
	}
	// Seeded: the next pass after the backoff sends them.
	f.mail.err = nil
	f.clock.now = f.clock.now.Add(RetryFirst)
	f.pass(t)
	if got := f.recipients(); !slices.Equal(got, []string{"fatih@example.test", "root@example.test"}) || len(f.queue.Snapshot()) != 0 {
		t.Fatalf("recipients %v, queue %+v", got, f.queue.Snapshot())
	}
}

// The count of a role mapping's Group is best effort (review of #241, F2):
// a tree that cannot be read leaves MemberCount empty, and ADMIN is told
// at once.
func TestNoticeOfARoleMappingGoesWithoutACountItCannotRead(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.rosters.failing = map[string]bool{"/UYELER": true}
	f.svc.GroupRolesChanged(context.Background(), identity.GroupRolesChange{
		Group: identity.Group{Path: "/UYELER"}, Added: []identity.ClientRole{{ClientID: "core", Role: "users:manage"}}, ActorID: f.act.String(),
	})
	report := f.pass(t)
	if got := f.recipients(); !slices.Equal(got, []string{"fatih@example.test", "root@example.test"}) || report.Notices.Failed != 0 {
		t.Fatalf("recipients %v, report %+v", got, report.Notices)
	}
	if v := f.mail.sent[0].vars; v["MemberCount"] != "" || v["AdminRole"] != "yes" {
		t.Fatalf("vars %v", v)
	}
	if len(f.queue.Snapshot()) != 0 {
		t.Fatalf("left %+v", f.queue.Snapshot())
	}
	if !strings.Contains(f.svc.Prometheus(), "skylab_privilege_notice_mail_count_errors_total 1") || strings.Contains(f.logs.String(), "UYELER") {
		t.Fatalf("metrics:\n%s\nlogs: %s", f.svc.Prometheus(), f.logs.String())
	}
}

// ADMIN's own roster, unlike the count, must be read: without it the
// change waits for a retry.
func TestNoticeOfARoleMappingWaitsForADMINsRoster(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.rosters.failing = map[string]bool{"/ADMIN": true}
	f.svc.GroupRolesChanged(context.Background(), identity.GroupRolesChange{
		Group: identity.Group{Path: "/UYELER"}, Added: []identity.ClientRole{{ClientID: "forms", Role: "viewer"}}, ActorID: f.act.String(),
	})
	report := f.pass(t)
	queued := f.queue.Snapshot()
	if report.Notices.Failed != 1 || len(queued) != 1 || queued[0].RecipientID != nil || len(f.mail.sent) != 0 {
		t.Fatalf("report %+v, queue %+v", report.Notices, queued)
	}
}

// A roster that cannot be read keeps the change waiting, unexpanded.
func TestNoticeRetriesARosterReadFailure(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.rosters.err = errors.New("keycloak down")
	f.svc.MembershipChanged(context.Background(), identity.MembershipChange{UserID: f.ada, Group: identity.Group{Path: "/UYELER/YK"}, Added: true, ActorID: f.act.String()})
	report := f.pass(t)
	queued := f.queue.Snapshot()
	if report.Notices.Failed != 1 || len(queued) != 1 || queued[0].RecipientID != nil || queued[0].Attempts != 1 {
		t.Fatalf("report %+v, queue %+v", report.Notices, queued)
	}
}

// SKYMAIL_PRIVILEGE_NOTICE_TEMPLATE_KEY empty: nothing is queued, and with
// the role mail off too a Privileged Group is not asked about at all.
func TestNoticeOff(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.mail.off = true
	f.membershipMail.off = map[Kind]bool{KindRole: true}
	if f.svc.Notifies(identity.Group{Path: "/UYELER/YK"}) {
		t.Fatal("a Privileged Group is notified with both mails off")
	}
	f.svc.GroupRolesChanged(context.Background(), identity.GroupRolesChange{Group: identity.Group{Path: "/UYELER"}, Added: []identity.ClientRole{{ClientID: "a", Role: "b"}}})
	if len(f.queue.Snapshot()) != 0 {
		t.Fatal("queued with the notice off")
	}
	if !strings.Contains(f.svc.Prometheus(), "skylab_privilege_notice_mail_enabled 0") {
		t.Fatal("metrics say on")
	}
	// The role mail off alone: the notice still asks about the Group.
	f.mail.off = false
	if !f.svc.Notifies(identity.Group{Path: "/UYELER/YK"}) {
		t.Fatal("notice on, Group not notified")
	}
	if f.svc.Notifies(identity.Group{Path: "/UYELER/ARGE/WEBLAB/ADMIN"}) != !f.membershipMail.off[KindTeam] {
		t.Fatal("a team named ADMIN follows the team mail")
	}
}

// A notice switched off after it was queued is dropped at send time.
func TestNoticeSwitchedOffAfterQueueing(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.svc.MembershipChanged(context.Background(), identity.MembershipChange{UserID: f.ada, Group: identity.Group{Path: "/UYELER/YK"}, Added: true, ActorID: f.act.String()})
	f.mail.off = true
	f.pass(t)
	if len(f.queue.Snapshot()) != 0 || len(f.mail.sent) != 0 {
		t.Fatalf("queue %+v sent %+v", f.queue.Snapshot(), f.mail.sent)
	}
	if !strings.Contains(f.svc.Prometheus(), `skylab_privilege_notice_mail_total{outcome="skipped_kind_off"} 1`) {
		t.Fatalf("metrics:\n%s", f.svc.Prometheus())
	}
}

func TestNoticeLogsAndMetricsCarryNoPersonalData(t *testing.T) {
	t.Parallel()
	f := newNoticeFixture(t)
	f.svc.MembershipChanged(context.Background(), identity.MembershipChange{UserID: f.ada, Group: identity.Group{Path: "/UYELER/YK"}, Added: true, ActorID: f.act.String()})
	f.svc.RolesPrecheckFailed()
	f.pass(t)
	out := f.logs.String() + f.svc.Prometheus()
	for _, secret := range []string{"ada", "Ada", "fatih", "/UYELER/YK", f.ada.String(), "YK"} {
		if strings.Contains(out, secret) {
			t.Fatalf("%q in logs or metrics:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "skylab_privilege_notice_mail_precheck_errors_total 1") || !strings.Contains(out, `skylab_privilege_notice_mail_enqueued_total{event="member_added"} 1`) {
		t.Fatalf("metrics:\n%s", out)
	}
}

func TestDirectoryRostersReadTheTree(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
		{ID: "g-baskan", Name: "BASKAN", Path: "/UYELER/YK/BASKAN"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
	} {
		dir.PutGroup(g)
	}
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	for _, p := range []uuid.UUID{a, b, c} {
		dir.PutUser(identity.Person{ID: p, Email: p.String() + "@example.test", Enabled: true})
	}
	ctx := context.Background()
	for _, m := range []struct {
		group string
		user  uuid.UUID
	}{{"g-yk", a}, {"g-baskan", a}, {"g-baskan", b}, {"g-arge", c}} {
		if err := dir.AddMember(ctx, m.group, m.user); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DirectoryRosters{Directory: dir}.MemberIDs(ctx, "/UYELER/YK")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !slices.Contains(got, a) || !slices.Contains(got, b) {
		t.Fatalf("members %v", got)
	}
	none, err := DirectoryRosters{Directory: dir}.MemberIDs(ctx, "/YK")
	if err != nil || len(none) != 0 {
		t.Fatalf("missing root: %v %v", none, err)
	}
}

func TestNewServiceNeedsTheNoticeParts(t *testing.T) {
	t.Parallel()
	_, err := NewService(Config{Queue: NewMemoryQueue(), People: &fakePeople{}, Groups: fakeGroups{}, Mail: &fakeMail{}, Notice: &NoticeConfig{Queue: NewMemoryNoticeQueue()}})
	if err == nil {
		t.Fatal("a notice without rosters and a mailer")
	}
}
