package teampagenotice_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/teampagenotice"
)

type consents map[uuid.UUID]bool

func (c consents) TeamPageDecisions(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if open, ok := c[id]; ok {
			out[id] = open
		}
	}
	return out, nil
}

type people map[uuid.UUID]teammail.Recipient

func (p people) Recipient(_ context.Context, id uuid.UUID) (teammail.Recipient, teammail.SkipReason, error) {
	r, ok := p[id]
	if !ok {
		return teammail.Recipient{}, teammail.SkipInactive, nil
	}
	return r, "", nil
}

type sent struct {
	to   string
	vars map[string]string
}

type mailer struct {
	sent []sent
	// cancel, when set, is called before the next refusal: the run is
	// stopped while the mail is on its way.
	cancel func()
	// fail answers the next sends to these addresses with this error.
	fail map[string]error
}

func (m *mailer) TeamPageNotice(_ context.Context, to, _ string, vars map[string]string) error {
	if err, ok := m.fail[to]; ok {
		delete(m.fail, to)
		if m.cancel != nil {
			m.cancel()
		}
		return err
	}
	m.sent = append(m.sent, sent{to: to, vars: vars})
	return nil
}

type ledger struct {
	mu   sync.Mutex
	rows map[uuid.UUID]time.Time
}

func (l *ledger) Notified(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if _, ok := l.rows[id]; ok {
			out[id] = true
		}
	}
	return out, nil
}

func (l *ledger) Claim(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.rows[id]; ok {
		return false, nil
	}
	l.rows[id] = at
	return true, nil
}

func (l *ledger) Release(ctx context.Context, id uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.rows, id)
	return nil
}

type fixture struct {
	cfg                                    teampagenotice.Config
	mail                                   *mailer
	ledger                                 *ledger
	ada, grace, kemal, consented, withdrew uuid.UUID
	gone                                   uuid.UUID
}

// newFixture: WEBLAB lists Ada, Grace, the one who consented, the one who
// withdrew and one who is gone; AGC lists Ada too; the Board's roster lists
// Kemal. Nobody but Ada, Grace and Kemal has decided nothing and can be
// written to.
func newFixture() *fixture {
	f := &fixture{
		mail: &mailer{fail: map[string]error{}}, ledger: &ledger{rows: map[uuid.UUID]time.Time{}},
		ada: uuid.New(), grace: uuid.New(), kemal: uuid.New(), consented: uuid.New(), withdrew: uuid.New(), gone: uuid.New(),
	}
	listings := []identity.TeamPageListing{
		{Team: identity.Group{Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: map[string]string{"display_name_tr": "Web Geliştirme"}},
			People: []uuid.UUID{f.ada, f.grace, f.consented, f.withdrew, f.gone}},
		{Team: identity.Group{Name: "AGC", Path: "/UYELER/ARGE/ALGOLAB/AGC"}, People: []uuid.UUID{f.ada}},
		{Team: identity.Group{Name: "YK", Path: "/UYELER/YK", Attributes: map[string]string{"display_name_tr": "Yönetim Kurulu"}},
			Board: true, People: []uuid.UUID{f.kemal}},
	}
	f.cfg = teampagenotice.Config{
		Listings: func(context.Context) ([]identity.TeamPageListing, error) { return listings, nil },
		Consents: consents{f.consented: true, f.withdrew: false},
		People: people{
			f.ada:       {Email: "ada@example.test", FullName: "Ada Lovelace", FirstName: "Ada"},
			f.grace:     {Email: "grace@example.test", FullName: "Grace Hopper", FirstName: "Grace"},
			f.kemal:     {Email: "kemal@example.test", FullName: "Kemal Board", FirstName: "Kemal"},
			f.consented: {Email: "c@example.test"}, f.withdrew: {Email: "w@example.test"},
		},
		Mail: f.mail, Ledger: f.ledger,
		HideAt:      time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC),
		SettingsURL: "https://my.yildizskylab.com/club-profile",
		Now:         func() time.Time { return time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC) },
	}
	return f
}

func (f *fixture) byAddress() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, s := range f.mail.sent {
		out[s.to] = s.vars
	}
	return out
}

func TestADryRunCountsAndSendsNothing(t *testing.T) {
	f := newFixture()
	report, err := teampagenotice.Run(context.Background(), f.cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	want := teampagenotice.Report{Rosters: 3, Listed: 6, Consented: 1, Withdrew: 1, Undecided: 4, Skipped: 1, Due: 3, DueBoard: 1}
	if report != want {
		t.Fatalf("report %+v, want %+v", report, want)
	}
	if len(f.mail.sent) != 0 || len(f.ledger.rows) != 0 {
		t.Fatalf("dry run sent %d, recorded %d", len(f.mail.sent), len(f.ledger.rows))
	}
}

// Only the undecided are told: who consented stays listed, who withdrew is
// hidden already. One mail per person names all their pages; the Board
// line goes to the Board's member alone. The second run sends none.
func TestApplySendsOnceToTheUndecided(t *testing.T) {
	f := newFixture()
	report, err := teampagenotice.Run(context.Background(), f.cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 3 || report.Failed != 0 || report.Unknown != 0 || report.Due != 3 {
		t.Fatalf("report %+v", report)
	}
	got := f.byAddress()
	if len(got) != 3 || got["c@example.test"] != nil || got["w@example.test"] != nil {
		t.Fatalf("sent to %v", got)
	}
	ada := got["ada@example.test"]
	if ada["Teams"] != "WEBLAB · Web Geliştirme, AGC" || ada["FirstName"] != "Ada" || ada["Board"] != "" ||
		ada["HideAt"] != "11.11.2026 00:00" || ada["SettingsURL"] != "https://my.yildizskylab.com/club-profile" {
		t.Fatalf("Ada's mail %v", ada)
	}
	if got["kemal@example.test"]["Board"] != "true" || got["kemal@example.test"]["Teams"] != "YK · Yönetim Kurulu" {
		t.Fatalf("Kemal's mail %v", got["kemal@example.test"])
	}
	if got["grace@example.test"]["Board"] != "" {
		t.Fatalf("Grace's mail %v", got["grace@example.test"])
	}

	again, err := teampagenotice.Run(context.Background(), f.cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Sent != 0 || again.Due != 0 || again.AlreadyNotified != 3 || len(f.mail.sent) != 3 {
		t.Fatalf("second run %+v, %d mails in all", again, len(f.mail.sent))
	}
}

// A refused mail surely did not go: its row goes and the next run sends
// it. One sent without an answer may have gone: its row stays.
func TestARefusedMailGoesNextRunAnUnansweredOneNever(t *testing.T) {
	f := newFixture()
	f.mail.fail["ada@example.test"] = &mail.SendError{Status: 503}
	f.mail.fail["grace@example.test"] = mail.ErrMaybeDelivered
	report, err := teampagenotice.Run(context.Background(), f.cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 1 || report.Failed != 1 || report.Unknown != 1 {
		t.Fatalf("report %+v", report)
	}
	if _, ok := f.ledger.rows[f.ada]; ok {
		t.Fatal("a refused mail stays recorded")
	}
	if _, ok := f.ledger.rows[f.grace]; !ok {
		t.Fatal("an unanswered mail is not recorded")
	}
	again, err := teampagenotice.Run(context.Background(), f.cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Sent != 1 || f.mail.sent[len(f.mail.sent)-1].to != "ada@example.test" {
		t.Fatalf("second run %+v", again)
	}
}

// A run stopped while a mail is on its way, before it reached SkyMail,
// still takes that person's row out: the next run tells them.
func TestAStoppedRunLeavesNoRowForAMailThatDidNotGo(t *testing.T) {
	f := newFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.mail.cancel = cancel
	f.mail.fail["ada@example.test"] = &mail.SendError{}
	report, err := teampagenotice.Run(ctx, f.cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 1 {
		t.Fatalf("report %+v", report)
	}
	if _, ok := f.ledger.rows[f.ada]; ok {
		t.Fatal("the stopped run left Ada recorded")
	}
}

func TestNoTemplateStopsTheRunAndRecordsNothing(t *testing.T) {
	f := newFixture()
	f.mail.fail["ada@example.test"] = mail.ErrTemplateUnconfigured
	if _, err := teampagenotice.Run(context.Background(), f.cfg, true); !errors.Is(err, mail.ErrTemplateUnconfigured) {
		t.Fatalf("err %v", err)
	}
	if len(f.ledger.rows) != 0 {
		t.Fatalf("recorded %v", f.ledger.rows)
	}
}

func TestAnUnknownTemplateStopsTheRun(t *testing.T) {
	f := newFixture()
	f.mail.fail["ada@example.test"] = &mail.SendError{Status: 404}
	report, err := teampagenotice.Run(context.Background(), f.cfg, true)
	if !errors.Is(err, teampagenotice.ErrTemplateMissing) || report.Sent != 0 || len(f.ledger.rows) != 0 {
		t.Fatalf("report %+v, err %v, recorded %v", report, err, f.ledger.rows)
	}
}

func TestWithoutATransitionOrAfterItNothingRuns(t *testing.T) {
	f := newFixture()
	f.cfg.HideAt = time.Time{}
	if _, err := teampagenotice.Run(context.Background(), f.cfg, false); !errors.Is(err, teampagenotice.ErrNoTransition) {
		t.Fatalf("unset: %v", err)
	}
	f.cfg.HideAt = f.cfg.Now()
	if _, err := teampagenotice.Run(context.Background(), f.cfg, true); !errors.Is(err, teampagenotice.ErrTransitionOver) {
		t.Fatalf("over: %v", err)
	}
	if len(f.mail.sent) != 0 {
		t.Fatal("sent")
	}
}
