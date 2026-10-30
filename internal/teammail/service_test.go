package teammail

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

type sent struct {
	kind                Kind
	recipient, fullName string
	vars                map[string]string
}

type fakeMail struct {
	sent []sent
	err  error
	// during runs inside the send, as SkyMail is answering.
	during func()
	// off are the kinds without a template key.
	off map[Kind]bool
}

func (m *fakeMail) MembershipConfigured(kind Kind) bool { return !m.off[kind] }

func (m *fakeMail) Membership(_ context.Context, kind Kind, recipient, fullName string, vars map[string]string) error {
	if m.during != nil {
		m.during()
	}
	if m.off[kind] {
		return mail.ErrTemplateUnconfigured
	}
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, sent{kind, recipient, fullName, vars})
	return nil
}

type fakePeople struct {
	recipients map[uuid.UUID]Recipient
	skip       map[uuid.UUID]SkipReason
	names      map[uuid.UUID]string
	err        error
}

func (p *fakePeople) Recipient(_ context.Context, id uuid.UUID) (Recipient, SkipReason, error) {
	if p.err != nil {
		return Recipient{}, "", p.err
	}
	if reason, ok := p.skip[id]; ok {
		return Recipient{}, reason, nil
	}
	r, ok := p.recipients[id]
	if !ok {
		return Recipient{}, SkipInactive, nil
	}
	return r, "", nil
}

func (p *fakePeople) DisplayName(_ context.Context, id uuid.UUID) string { return p.names[id] }

type fakeGroups map[string]identity.Group

func (g fakeGroups) GetGroup(_ context.Context, ref string) (identity.Group, error) {
	if group, ok := g[ref]; ok {
		return group, nil
	}
	return identity.Group{}, identity.ErrNotFound
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

type fixture struct {
	svc    *Service
	queue  *MemoryQueue
	mail   *fakeMail
	people *fakePeople
	clock  *clock
	logs   *bytes.Buffer
	ada    uuid.UUID
	fatih  uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		queue: NewMemoryQueue(), mail: &fakeMail{}, clock: &clock{now: time.Date(2026, 10, 9, 22, 30, 0, 0, time.UTC)},
		logs: &bytes.Buffer{}, ada: uuid.New(), fatih: uuid.New(),
	}
	f.people = &fakePeople{
		recipients: map[uuid.UUID]Recipient{f.ada: {Email: "ada@example.test", FullName: "Ada Lovelace"}},
		skip:       map[uuid.UUID]SkipReason{},
		names:      map[uuid.UUID]string{f.fatih: "Fatih Naz"},
	}
	svc, err := NewService(Config{
		Queue: f.queue, People: f.people, Mail: f.mail, Now: f.clock.Now, Logger: log.New(f.logs, "", 0),
		Groups: fakeGroups{
			"/UYELER/ORGANIZASYON/ARTLAB": {
				Name: "ARTLAB", Path: "/UYELER/ORGANIZASYON/ARTLAB", Attributes: map[string]string{"display_name_tr": "Tasarım & Sanat"},
			},
			"/UYELER/YK":   {Name: "YK", Path: "/UYELER/YK", Attributes: map[string]string{"display_name_tr": "Yönetim Kurulu"}},
			"/UYELER/ARGE": {Name: "ARGE", Path: "/UYELER/ARGE", Attributes: map[string]string{"display_name_tr": "Ar-Ge"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func (f *fixture) change(t *testing.T, path string, added bool, actor string) {
	t.Helper()
	f.svc.MembershipChanged(context.Background(), identity.MembershipChange{
		UserID: f.ada, Group: identity.Group{Path: path}, Added: added, ActorID: actor,
	})
}

// Every Group's membership is mailed (Yusuf, 2026-10-12): teams, the
// Privileged Groups and every other Group.
func TestServiceNotifiesEveryGroup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, path := range []string{"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/YK", "/UYELER/DK/UYE", "/ADMIN", "/UYELER/ARGE", "/UYELER", "/UYELER/ESKI-EDITORLER"} {
		if !f.svc.Notifies(identity.Group{Path: path}) {
			t.Errorf("%s not notified", path)
		}
	}
	if f.svc.Notifies(identity.Group{}) {
		t.Fatal("a Group without a path notified")
	}
}

// A kind whose template key is empty is off on its own: its changes are
// neither read for nor queued, and the other kinds go on.
func TestServiceLeavesAKindThatIsOffAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.mail.off = map[Kind]bool{KindGroup: true}
	if f.svc.Notifies(identity.Group{Path: "/UYELER/ARGE"}) {
		t.Fatal("an area notified with the group mail off")
	}
	if !f.svc.Notifies(identity.Group{Path: "/UYELER/YK"}) || !f.svc.Notifies(identity.Group{Path: "/UYELER/ARGE/WEBLAB"}) {
		t.Fatal("the other kinds stopped")
	}
	f.change(t, "/UYELER/ARGE", true, "")
	if q := f.queue.Snapshot(); len(q) != 0 {
		t.Fatalf("queued %+v", q)
	}
	text := f.svc.Prometheus()
	for _, line := range []string{
		`skylab_team_membership_mail_kind_enabled{mail_kind="team"} 1`,
		`skylab_team_membership_mail_kind_enabled{mail_kind="role"} 1`,
		`skylab_team_membership_mail_kind_enabled{mail_kind="group"} 0`,
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q in\n%s", line, text)
		}
	}
}

// A Privileged Group's change gets the role mail: the Group's name, which
// root it is (Role) and who made it.
func TestServiceSendsTheRoleMail(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/YK", true, f.fatih.String())
	if q := f.queue.Snapshot(); len(q) != 1 || q[0].Kind != KindRole {
		t.Fatalf("queued %+v", q)
	}
	report, err := f.svc.Pass(context.Background())
	if err != nil || report.Sent != 1 || len(f.mail.sent) != 1 {
		t.Fatalf("report %+v err %v sent %v", report, err, f.mail.sent)
	}
	m := f.mail.sent[0]
	want := map[string]string{
		"GroupName": "YK · Yönetim Kurulu", "Role": "board", "Action": "added",
		"EffectiveAt": "10.10.2026", "ActorName": "Fatih Naz",
	}
	if m.kind != KindRole || m.recipient != "ada@example.test" || len(m.vars) != len(want) {
		t.Fatalf("sent %+v", m)
	}
	for k, v := range want {
		if m.vars[k] != v {
			t.Fatalf("%s = %q, want %q (vars %v)", k, m.vars[k], v, m.vars)
		}
	}
	text := f.svc.Prometheus()
	for _, line := range []string{
		`skylab_team_membership_mail_total{outcome="sent"} 1`,
		`skylab_team_membership_mail_by_kind_total{mail_kind="role",outcome="sent"} 1`,
		`skylab_team_membership_mail_by_kind_total{mail_kind="team",outcome="sent"} 0`,
		`skylab_team_membership_mail_enqueued_total 1`,
		`skylab_team_membership_mail_enqueued_by_kind_total{mail_kind="role"} 1`,
		// Kept for dashboards that read it; nothing is skipped as no team now.
		`skylab_team_membership_mail_total{outcome="skipped_not_team"} 0`,
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q in\n%s", line, text)
		}
	}
	if strings.Contains(text, "YK") || strings.Contains(f.logs.String(), "YK") {
		t.Fatalf("a Group named in\n%s%s", text, f.logs.String())
	}
}

// Any other Group's change gets the group mail. A subgroup under a root is
// still the role mail, named by its own Group.
func TestServiceSendsTheGroupMail(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ARGE", false, "")
	f.svc.MembershipChanged(context.Background(), identity.MembershipChange{
		UserID: f.ada, Group: identity.Group{Path: "/UYELER/DK/UYE"}, Added: true,
	})
	if _, err := f.svc.Pass(context.Background()); err != nil || len(f.mail.sent) != 2 {
		t.Fatalf("err %v sent %v", err, f.mail.sent)
	}
	group, role := f.mail.sent[0], f.mail.sent[1]
	if group.kind != KindGroup || group.vars["GroupName"] != "ARGE · Ar-Ge" || group.vars["Action"] != "removed" ||
		group.vars["ActorName"] != "" || group.vars["EffectiveAt"] != "10.10.2026" || len(group.vars) != 4 {
		t.Fatalf("group mail %+v", group)
	}
	// The Group read failed: the last segment of its path stands for it.
	if role.kind != KindRole || role.vars["GroupName"] != "UYE" || role.vars["Role"] != "audit" || role.vars["Action"] != "added" {
		t.Fatalf("role mail %+v", role)
	}
	if !strings.Contains(f.svc.Prometheus(), `skylab_team_membership_mail_by_kind_total{mail_kind="group",outcome="sent"} 1`) {
		t.Fatalf("metrics\n%s", f.svc.Prometheus())
	}
}

func TestServiceQueuesAndSendsTheChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ORGANIZASYON/ARTLAB/LIDERLER", true, f.fatih.String())

	queued := f.queue.Snapshot()
	if len(queued) != 1 {
		t.Fatalf("queued %v", queued)
	}
	got := queued[0]
	if got.SubjectID != f.ada || got.ActorID == nil || *got.ActorID != f.fatih || got.GroupPath != "/UYELER/ORGANIZASYON/ARTLAB/LIDERLER" ||
		got.Action != ActionAdded || !got.OccurredAt.Equal(f.clock.now) {
		t.Fatalf("queued %+v", got)
	}

	report, err := f.svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 1 || len(f.mail.sent) != 1 || len(f.queue.Snapshot()) != 0 {
		t.Fatalf("report %+v sent %v queue %v", report, f.mail.sent, f.queue.Snapshot())
	}
	m := f.mail.sent[0]
	if m.recipient != "ada@example.test" || m.fullName != "Ada Lovelace" {
		t.Fatalf("recipient %q %q", m.recipient, m.fullName)
	}
	want := map[string]string{
		"TeamName": "ARTLAB · Tasarım & Sanat", "Action": "added", "Role": "leader",
		// 22:30 UTC on the 9th is past midnight in Istanbul.
		"EffectiveAt": "10.10.2026", "LeaderName": "Fatih Naz",
	}
	for k, v := range want {
		if m.vars[k] != v {
			t.Fatalf("%s = %q, want %q (vars %v)", k, m.vars[k], v, m.vars)
		}
	}
}

func TestServiceRemovalWithUnknownActorAndGroup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// A service account or a principal without a UUID subject has no name.
	f.change(t, "/UYELER/ARGE/GAMELAB", false, "service-account-x")
	if got := f.queue.Snapshot(); len(got) != 1 || got[0].ActorID != nil || got[0].Action != ActionRemoved {
		t.Fatalf("queued %+v", got)
	}
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.mail.sent) != 1 {
		t.Fatalf("sent %v", f.mail.sent)
	}
	vars := f.mail.sent[0].vars
	// The Group read failed: its own name stands for it.
	if vars["TeamName"] != "GAMELAB" || vars["Action"] != "removed" || vars["LeaderName"] != "" || vars["Role"] != "member" {
		t.Fatalf("vars %v", vars)
	}
}

func TestServiceSkipsPeopleWhoGetNoMail(t *testing.T) {
	t.Parallel()
	for _, reason := range []SkipReason{SkipInactive, SkipNoEmail} {
		f := newFixture(t)
		f.people.skip[f.ada] = reason
		f.change(t, "/UYELER/ARGE/WEBLAB", true, f.fatih.String())
		report, err := f.svc.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(f.mail.sent) != 0 || len(f.queue.Snapshot()) != 0 || report.Skipped != 1 {
			t.Fatalf("%s: report %+v sent %v queue %v", reason, report, f.mail.sent, f.queue.Snapshot())
		}
		if !strings.Contains(f.svc.Prometheus(), `skylab_team_membership_mail_total{outcome="skipped_`+string(reason)+`"} 1`) {
			t.Fatalf("%s: metrics\n%s", reason, f.svc.Prometheus())
		}
	}
}

func TestServiceRetriesWithBackoffAndDropsPermanentRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ARGE/WEBLAB", true, "")

	// SkyMail down: the row waits for its backoff, nothing is lost.
	f.mail.err = &mail.SendError{Status: 503}
	report, err := f.svc.Pass(context.Background())
	if err != nil || report.Failed != 1 {
		t.Fatalf("report %+v err %v", report, err)
	}
	queued := f.queue.Snapshot()
	if len(queued) != 1 || queued[0].Attempts != 1 || !queued[0].NextAttemptAt.Equal(f.clock.now.Add(RetryFirst)) {
		t.Fatalf("queued %+v", queued)
	}
	// Not due yet: the next pass leaves it alone.
	if report, _ := f.svc.Pass(context.Background()); report.Failed != 0 || report.Sent != 0 {
		t.Fatalf("early pass %+v", report)
	}
	// A failed lookup waits the same way.
	f.clock.now = f.clock.now.Add(RetryFirst)
	f.people.err = errors.New("keycloak down")
	if report, _ := f.svc.Pass(context.Background()); report.Failed != 1 {
		t.Fatalf("lookup failure %+v", report)
	}
	if q := f.queue.Snapshot(); len(q) != 1 || q[0].Attempts != 2 || !q[0].NextAttemptAt.Equal(f.clock.now.Add(2*RetryFirst)) {
		t.Fatalf("queued %+v", q)
	}
	// A body SkyMail refuses as such is dropped.
	f.people.err = nil
	f.clock.now = f.clock.now.Add(2 * RetryFirst)
	f.mail.err = &mail.SendError{Status: 400}
	if report, _ := f.svc.Pass(context.Background()); report.Rejected != 1 {
		t.Fatalf("refusal %+v", report)
	}
	if q := f.queue.Snapshot(); len(q) != 0 {
		t.Fatalf("queued %+v", q)
	}
}

func TestServiceGivesUpOnOldChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ARGE/WEBLAB", true, "")
	f.mail.err = &mail.SendError{}
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.clock.now = f.clock.now.Add(GiveUpAfter + time.Second)
	report, err := f.svc.Pass(context.Background())
	if err != nil || report.Expired != 1 || len(f.queue.Snapshot()) != 0 {
		t.Fatalf("report %+v err %v queue %v", report, err, f.queue.Snapshot())
	}
}

type brokenQueue struct{ *MemoryQueue }

func (brokenQueue) Enqueue(context.Context, Change) (Enqueued, error) {
	return "", errors.New("database is down")
}

func TestServiceQueueFailureNeverReachesTheCaller(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	svc, err := NewService(Config{
		Queue: brokenQueue{NewMemoryQueue()}, People: &fakePeople{}, Mail: &fakeMail{}, Groups: fakeGroups{},
		Logger: log.New(&logs, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ada := uuid.New()
	svc.MembershipChanged(context.Background(), identity.MembershipChange{UserID: ada, Group: identity.Group{Path: "/UYELER/ARGE/WEBLAB"}, Added: true})
	if !strings.Contains(svc.Prometheus(), "skylab_team_membership_mail_queue_errors_total 1") {
		t.Fatalf("metrics\n%s", svc.Prometheus())
	}
	if line := logs.String(); line == "" || strings.Contains(line, ada.String()) || strings.Contains(line, "WEBLAB") {
		t.Fatalf("log %q", line)
	}
}

func TestServiceLogsAndMetricsCarryNoPersonalData(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ORGANIZASYON/ARTLAB", true, f.fatih.String())
	f.mail.err = &mail.SendError{Status: 503}
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mail.err = nil
	f.clock.now = f.clock.now.Add(time.Hour)
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	text := f.svc.Prometheus() + f.logs.String()
	for _, secret := range []string{"ada@", "Ada", "Fatih", "ARTLAB", f.ada.String(), f.fatih.String()} {
		if strings.Contains(text, secret) {
			t.Fatalf("%q in\n%s", secret, text)
		}
	}
	for _, line := range []string{
		"skylab_team_membership_mail_enabled 1",
		`skylab_team_membership_mail_total{outcome="sent"} 1`,
		`skylab_team_membership_mail_total{outcome="failed"} 1`,
		"skylab_team_membership_mail_enqueued_total 1",
		"skylab_team_membership_mail_backlog 0",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q in\n%s", line, text)
		}
	}
	if !strings.Contains(Off{}.Prometheus(), "skylab_team_membership_mail_enabled 0") {
		t.Fatalf("off metrics %q", Off{}.Prometheus())
	}
}

func TestNewServiceNeedsItsParts(t *testing.T) {
	t.Parallel()
	if _, err := NewService(Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
}

func TestServiceNamesTheTeamByCodeAndTurkishName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ORGANIZASYON/ARTLAB", true, "")
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.mail.sent) != 1 || f.mail.sent[0].vars["TeamName"] != "ARTLAB · Tasarım & Sanat" || f.mail.sent[0].vars["Role"] != "member" {
		t.Fatalf("sent %v", f.mail.sent)
	}
}

// limitSpy records how many changes each claim asks for.
type limitSpy struct {
	*MemoryQueue
	limits []int
}

func (q *limitSpy) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Change, error) {
	q.limits = append(q.limits, limit)
	return q.MemoryQueue.Claim(ctx, now, lease, limit)
}

// One change per claim: a lease covers one send, never a batch of them that
// could outlast it while another core takes the rest.
func TestServiceClaimsOneChangeAtATime(t *testing.T) {
	t.Parallel()
	queue := &limitSpy{MemoryQueue: NewMemoryQueue()}
	ada := uuid.New()
	people := &fakePeople{recipients: map[uuid.UUID]Recipient{ada: {Email: "ada@example.test"}}}
	mailer := &fakeMail{}
	svc, err := NewService(Config{Queue: queue, People: people, Mail: mailer, Groups: fakeGroups{}, Logger: log.New(&bytes.Buffer{}, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/AIRLAB", "/UYELER/ARGE/GAMELAB"} {
		svc.MembershipChanged(context.Background(), identity.MembershipChange{UserID: ada, Group: identity.Group{Path: path}, Added: true})
	}
	report, err := svc.Pass(context.Background())
	if err != nil || report.Sent != 3 {
		t.Fatalf("report %+v err %v", report, err)
	}
	for _, limit := range queue.limits {
		if limit != 1 {
			t.Fatalf("claim limits %v", queue.limits)
		}
	}
	if lease < sendTimeout+30*time.Second {
		t.Fatalf("lease %s does not cover one send (%s)", lease, sendTimeout)
	}
}

// Shutting down mid-send is no failure: the row keeps its attempts and is
// taken again once its lease runs out, and nothing says it failed.
func TestServiceShutdownDuringASendIsNotAFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ARGE/WEBLAB", true, "")
	ctx, cancel := context.WithCancel(context.Background())
	f.mail.during = cancel
	f.mail.err = &mail.SendError{}
	report, err := f.svc.Pass(ctx)
	if err == nil || report.Failed != 0 {
		t.Fatalf("report %+v err %v", report, err)
	}
	if q := f.queue.Snapshot(); len(q) != 1 || q[0].Attempts != 0 {
		t.Fatalf("queued %+v", q)
	}
	if strings.Contains(f.svc.Prometheus(), `outcome="failed"} 1`) {
		t.Fatalf("metrics\n%s", f.svc.Prometheus())
	}
}

// A mail SkyMail took is done with, even when core is stopping as it
// answers: the row is deleted so it is not sent twice.
func TestServiceCompletesASentMailDuringShutdown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ARGE/WEBLAB", true, "")
	ctx, cancel := context.WithCancel(context.Background())
	f.mail.during = cancel
	report, _ := f.svc.Pass(ctx)
	if report.Sent != 1 || len(f.queue.Snapshot()) != 0 {
		t.Fatalf("report %+v queued %+v", report, f.queue.Snapshot())
	}
}

// A row an older core queued carries no kind (the column's default says
// team): the sending core's rule decides its mail. A kind switched off since
// the row was queued skips it.
func TestServiceSendsAQueuedRowByTheCurrentRule(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, path := range []string{"/UYELER/YK", "/UYELER/ARGE"} {
		if _, err := f.queue.Enqueue(context.Background(), Change{
			ID: uuid.New(), SubjectID: f.ada, GroupPath: path, Action: ActionAdded, OccurredAt: f.clock.now, Kind: KindTeam,
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.mail.off = map[Kind]bool{KindGroup: true}
	report, err := f.svc.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 1 || report.Skipped != 1 || len(f.mail.sent) != 1 || f.mail.sent[0].kind != KindRole || len(f.queue.Snapshot()) != 0 {
		t.Fatalf("report %+v sent %+v", report, f.mail.sent)
	}
	text := f.svc.Prometheus()
	for _, line := range []string{
		`skylab_team_membership_mail_total{outcome="skipped_kind_off"} 1`,
		`skylab_team_membership_mail_by_kind_total{mail_kind="group",outcome="skipped_kind_off"} 1`,
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q in\n%s", line, text)
		}
	}
}

// The same change queued twice while the first waits (a double click) is
// one mail.
func TestServiceQueuesAPendingChangeOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ARGE/WEBLAB", true, "")
	f.change(t, "/UYELER/ARGE/WEBLAB", true, "")
	if q := f.queue.Snapshot(); len(q) != 1 {
		t.Fatalf("queued %+v", q)
	}
	text := f.svc.Prometheus()
	for _, line := range []string{
		"skylab_team_membership_mail_enqueued_total 1",
		`skylab_team_membership_mail_coalesced_total{kind="duplicate"} 1`,
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q in\n%s", line, text)
		}
	}
}

// Add, remove and add again before any mail went: the first two cancel out,
// and the one mail says what holds now.
func TestServiceCancelsOppositeChangesBeforeTheyAreSent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ARGE/WEBLAB", true, "")
	f.change(t, "/UYELER/ARGE/WEBLAB", false, "")
	if q := f.queue.Snapshot(); len(q) != 0 {
		t.Fatalf("queued %+v", q)
	}
	f.change(t, "/UYELER/ARGE/WEBLAB", true, "")
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.mail.sent) != 1 || f.mail.sent[0].vars["Action"] != "added" {
		t.Fatalf("sent %v", f.mail.sent)
	}
	text := f.svc.Prometheus()
	for _, line := range []string{
		"skylab_team_membership_mail_enqueued_total 2",
		`skylab_team_membership_mail_coalesced_total{kind="cancelled"} 1`,
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q in\n%s", line, text)
		}
	}
}

func TestMemoryQueueCancelsOnlyUnclaimedOppositeChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := NewMemoryQueue()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	subject := uuid.New()
	change := func(action Action) Change {
		return Change{ID: uuid.New(), SubjectID: subject, GroupPath: "/UYELER/ARGE/WEBLAB", Action: action, OccurredAt: now}
	}
	if got, _ := q.Enqueue(ctx, change(ActionAdded)); got != EnqueuedQueued {
		t.Fatalf("add %v", got)
	}
	claimed, _ := q.Claim(ctx, now, time.Minute, 1)
	if got, _ := q.Enqueue(ctx, change(ActionRemoved)); got != EnqueuedQueued || len(q.Snapshot()) != 2 {
		t.Fatalf("removal of a claimed add: %v %+v", got, q.Snapshot())
	}
	if err := q.Retry(ctx, claimed[0], now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Both now wait unclaimed (the add in its backoff). Adding again cancels
	// the waiting removal, which leaves the add.
	if got, _ := q.Enqueue(ctx, change(ActionAdded)); got != EnqueuedCancelled || len(q.Snapshot()) != 1 || q.Snapshot()[0].Action != ActionAdded {
		t.Fatalf("re-add: %v %+v", got, q.Snapshot())
	}
	if got, _ := q.Enqueue(ctx, change(ActionAdded)); got != EnqueuedDuplicate {
		t.Fatalf("add again: %v", got)
	}
	// The add failed once and SkyMail may have taken it: a removal does not
	// cancel it but is queued, and the next claim drops the add for it.
	removal := change(ActionRemoved)
	removal.OccurredAt = now.Add(time.Second)
	if got, _ := q.Enqueue(ctx, removal); got != EnqueuedQueued || len(q.Snapshot()) != 2 {
		t.Fatalf("removal: %v %+v", got, q.Snapshot())
	}
	if got, _ := q.Claim(ctx, now.Add(2*time.Second), time.Minute, 10); len(got) != 1 || got[0].ID != removal.ID || len(q.Snapshot()) != 1 {
		t.Fatalf("claimed %+v left %+v", got, q.Snapshot())
	}
}

func TestServiceCountsMembershipReadFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.svc.PrecheckFailed()
	if !strings.Contains(f.svc.Prometheus(), "skylab_team_membership_mail_precheck_errors_total 1") {
		t.Fatalf("metrics\n%s", f.svc.Prometheus())
	}
	if line := f.logs.String(); !strings.Contains(line, "team membership mail:") {
		t.Fatalf("log %q", line)
	}
}

func TestServiceTellsTheCoordinatorRole(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.change(t, "/UYELER/ORGANIZASYON/ARTLAB/KOORDINATORLER", false, "")
	if _, err := f.svc.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.mail.sent) != 1 || f.mail.sent[0].vars["Role"] != "coordinator" || f.mail.sent[0].vars["TeamName"] != "ARTLAB · Tasarım & Sanat" {
		t.Fatalf("sent %v", f.mail.sent)
	}
}

// A row keeps the Group's id (ADR 0065 ticket 07): a Team moved, or given
// a new code, before its mail went is found by it, and the mail names the
// Team where it is now; a row without one, or whose id Keycloak no longer
// knows, is read by its path as before.
func TestServiceFindsAMovedTeamByItsID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	moved := "8a7f6a8e-3f0b-4c63-9f11-5a3b2c9d1e00"
	f.svc.MembershipChanged(context.Background(), identity.MembershipChange{
		UserID: f.ada, Group: identity.Group{ID: moved, Path: "/UYELER/ARGE/ALGOLAB/AGC/LIDERLER"}, Added: true, ActorID: f.fatih.String(),
	})
	if q := f.queue.Snapshot(); len(q) != 1 || q[0].GroupID != moved {
		t.Fatalf("queued %+v", q)
	}
	// Moved under ARTLAB and renamed meanwhile.
	f.svc.groups = fakeGroups{
		moved:                              {ID: moved, Name: "LIDERLER", Path: "/UYELER/ORGANIZASYON/ARTLAB/AGC2/LIDERLER"},
		"/UYELER/ORGANIZASYON/ARTLAB/AGC2": {Name: "AGC2", Path: "/UYELER/ORGANIZASYON/ARTLAB/AGC2", Attributes: map[string]string{"display_name_tr": "Algoritma"}},
	}
	report, err := f.svc.Pass(context.Background())
	if err != nil || report.Sent != 1 || len(f.mail.sent) != 1 {
		t.Fatalf("report %+v err %v", report, err)
	}
	if m := f.mail.sent[0]; m.vars["TeamName"] != "AGC2 · Algoritma" || m.vars["Role"] != "leader" {
		t.Fatalf("vars %v", m.vars)
	}

	g := newFixture(t)
	g.svc.MembershipChanged(context.Background(), identity.MembershipChange{
		UserID: g.ada, Group: identity.Group{ID: moved, Path: "/UYELER/ORGANIZASYON/ARTLAB"}, Added: true,
	})
	if _, err := g.svc.Pass(context.Background()); err != nil || len(g.mail.sent) != 1 || g.mail.sent[0].vars["TeamName"] != "ARTLAB · Tasarım & Sanat" {
		t.Fatalf("an id Keycloak does not know: %v %+v", err, g.mail.sent)
	}
}
