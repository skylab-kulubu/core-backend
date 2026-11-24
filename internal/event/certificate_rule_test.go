package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/optional"
)

// The certificate rule's threshold and Session marks (certificate × session
// ticket 01; docs/certificate-rule.md).

type ruleEnv struct {
	store *event.MemoryStore
	svc   event.Service
	ev    event.Event
	day   event.Day
}

func newRuleEnv(t *testing.T) ruleEnv {
	t.Helper()
	ctx := context.Background()
	env := ruleEnv{store: event.NewMemoryStore()}
	env.svc = event.NewService(env.store, authz.NewAuthorizer(authz.DefaultPolicy()))
	var err error
	if env.ev, err = env.svc.Create(ctx, copyLeader, event.Event{Name: "ARTLAB", Location: "YTÜ", OwnerTeam: "WEBLAB", AttendanceRule: "count"}); err != nil {
		t.Fatal(err)
	}
	if env.day, err = env.svc.CreateDay(ctx, copyLeader, event.Day{EventID: env.ev.ID, Name: "Cumartesi"}); err != nil {
		t.Fatal(err)
	}
	return env
}

func (env ruleEnv) session(t *testing.T, title, sessionType string, marks event.SessionCertificate) event.Session {
	t.Helper()
	sess, err := env.svc.CreateSession(context.Background(), copyLeader, event.Session{
		EventDayID: env.day.ID, Title: title, SpeakerName: "Ada", SessionType: sessionType,
	}.WithCertificate(marks))
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func mark(v bool) optional.Field[bool] { return optional.Field[bool]{Set: true, Value: v} }

func TestSessionCertificateMarksDefaultsAndKeep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRuleEnv(t)
	talk := env.session(t, "Açılış", "TALK", event.SessionCertificate{})
	if !talk.CertificateCounted || talk.CertificateRequired {
		t.Fatalf("talk %+v", talk)
	}
	networking := env.session(t, "Tanışma", "NETWORKING", event.SessionCertificate{})
	if networking.CertificateCounted || networking.CertificateRequired {
		t.Fatalf("networking %+v", networking)
	}
	countedNetworking := env.session(t, "Tanışma 2", "networking", event.SessionCertificate{Counted: mark(true)})
	if !countedNetworking.CertificateCounted {
		t.Fatalf("networking counted by the body %+v", countedNetworking)
	}
	final := env.session(t, "Final", "TALK", event.SessionCertificate{Required: mark(true)})
	if !final.CertificateCounted || !final.CertificateRequired {
		t.Fatalf("required %+v", final)
	}
	if _, err := env.svc.CreateSession(ctx, copyLeader, event.Session{
		EventDayID: env.day.ID, Title: "Ara", SpeakerName: "Ada", SessionType: "OTHER",
	}.WithCertificate(event.SessionCertificate{Counted: mark(false), Required: mark(true)})); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("required, not counted: %v", err)
	}
	if _, err := env.svc.CreateSession(ctx, copyLeader, event.Session{
		EventDayID: env.day.ID, Title: "Ara", SpeakerName: "Ada", SessionType: "OTHER",
	}.WithCertificate(event.SessionCertificate{Counted: optional.Field[bool]{Set: true, Null: true}})); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("null mark: %v", err)
	}

	// An update that sends no mark keeps them, whatever the type becomes.
	kept, err := env.svc.UpdateSession(ctx, copyLeader, final.ID, event.Session{Title: "Final", SpeakerName: "Can", SessionType: "NETWORKING"}, nil, event.SessionFeedback{})
	if err != nil || !kept.CertificateCounted || !kept.CertificateRequired {
		t.Fatalf("kept %+v %v", kept, err)
	}
	// Uncounting a required Session alone is refused; with its required
	// mark off it is written.
	if _, err := env.svc.UpdateSession(ctx, copyLeader, final.ID, kept.WithCertificate(event.SessionCertificate{Counted: mark(false)}), nil, event.SessionFeedback{}); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("uncounting a required Session: %v", err)
	}
	off, err := env.svc.UpdateSession(ctx, copyLeader, final.ID, kept.WithCertificate(event.SessionCertificate{Counted: mark(false), Required: mark(false)}), nil, event.SessionFeedback{})
	if err != nil || off.CertificateCounted || off.CertificateRequired {
		t.Fatalf("off %+v %v", off, err)
	}
}

func TestCertificateRuleReadAndWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRuleEnv(t)
	var sessions []event.Session
	for _, title := range []string{"1", "2", "3", "4", "5", "6", "7", "8"} {
		sessions = append(sessions, env.session(t, title, "TALK", event.SessionCertificate{}))
	}
	networking := env.session(t, "Tanışma", "NETWORKING", event.SessionCertificate{})
	cancelled := env.session(t, "İptal", "TALK", event.SessionCertificate{})
	yes := true
	if _, err := env.svc.UpdateSession(ctx, copyLeader, cancelled.ID, cancelled, &yes, event.SessionFeedback{}); err != nil {
		t.Fatal(err)
	}

	rule, err := env.svc.CertificateRule(ctx, copyLeader, env.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 8 counted live Sessions: the networking one is not counted, the
	// cancelled one is not live. The default threshold is 6.
	if rule.Rule != "count" || rule.MinCount != nil || rule.CountedTotal != 8 || rule.EffectiveMin != 6 || len(rule.Sessions) != 10 {
		t.Fatalf("rule %+v", rule)
	}
	for _, s := range rule.Sessions {
		if s.DayID != env.day.ID || (s.ID == networking.ID && s.Counted) || (s.ID == cancelled.ID && !s.Cancelled) {
			t.Fatalf("session %+v", s)
		}
	}

	// Three uncounted, the first required, M = 6 over N = 5: kept, and
	// applied as 5. A Session it does not list keeps its marks.
	six := 6
	written, err := env.svc.SetCertificateRule(ctx, copyLeader, env.ev.ID, event.CertificateRuleInput{
		Rule: "count", MinCount: &six,
		Sessions: []event.SessionCertificateFlags{
			{ID: sessions[0].ID, Counted: true, Required: true},
			{ID: sessions[5].ID}, {ID: sessions[6].ID}, {ID: sessions[7].ID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if written.MinCount == nil || *written.MinCount != 6 || written.CountedTotal != 5 || written.EffectiveMin != 5 {
		t.Fatalf("written %+v", written)
	}
	stored, err := env.store.Get(ctx, env.ev.ID)
	if err != nil || stored.AttendanceRule != "count" || stored.AttendanceMinCount == nil || *stored.AttendanceMinCount != 6 {
		t.Fatalf("stored %+v %v", stored, err)
	}
	first, _ := env.store.GetSession(ctx, sessions[0].ID)
	second, _ := env.store.GetSession(ctx, sessions[1].ID)
	last, _ := env.store.GetSession(ctx, sessions[7].ID)
	if !first.CertificateRequired || !first.CertificateCounted || !second.CertificateCounted || second.CertificateRequired || last.CertificateCounted {
		t.Fatalf("marks %+v %+v %+v", first, second, last)
	}

	// The once rule drops the threshold.
	once, err := env.svc.SetCertificateRule(ctx, copyLeader, env.ev.ID, event.CertificateRuleInput{Rule: "once", MinCount: &six})
	if err != nil || once.Rule != "once" || once.MinCount != nil || once.EffectiveMin != 1 {
		t.Fatalf("once %+v %v", once, err)
	}
}

// Every refused write writes nothing: neither the rule nor any mark.
func TestCertificateRuleRefusalsWriteNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRuleEnv(t)
	a := env.session(t, "A", "TALK", event.SessionCertificate{})
	b := env.session(t, "B", "TALK", event.SessionCertificate{})
	archived := env.session(t, "Silinen", "TALK", event.SessionCertificate{})
	if err := env.svc.DeleteSession(ctx, copyLeader, archived.ID); err != nil {
		t.Fatal(err)
	}
	other := newRuleEnv(t)
	foreign := other.session(t, "Başka", "TALK", event.SessionCertificate{})
	if _, err := env.store.CreateSession(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	zero, two := 0, 2
	for name, in := range map[string]event.CertificateRuleInput{
		"another Event's Session": {Rule: "count", MinCount: &two, Sessions: []event.SessionCertificateFlags{{ID: a.ID, Counted: false}, {ID: foreign.ID, Counted: true}}},
		"an archived Session":     {Rule: "count", MinCount: &two, Sessions: []event.SessionCertificateFlags{{ID: a.ID, Counted: false}, {ID: archived.ID, Counted: true}}},
		"an unknown Session":      {Rule: "count", Sessions: []event.SessionCertificateFlags{{ID: a.ID}, {ID: uuid.New(), Counted: true}}},
		"required, not counted":   {Rule: "count", Sessions: []event.SessionCertificateFlags{{ID: a.ID}, {ID: b.ID, Required: true}}},
		"listed twice":            {Rule: "count", Sessions: []event.SessionCertificateFlags{{ID: a.ID}, {ID: a.ID, Counted: true}}},
		"threshold 0":             {Rule: "count", MinCount: &zero, Sessions: []event.SessionCertificateFlags{{ID: a.ID}}},
		"ratio":                   {Rule: "ratio", Sessions: []event.SessionCertificateFlags{{ID: a.ID}}},
		"no rule":                 {Sessions: []event.SessionCertificateFlags{{ID: a.ID}}},
	} {
		if _, err := env.svc.SetCertificateRule(ctx, copyLeader, env.ev.ID, in); !errors.Is(err, event.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
		stored, _ := env.store.Get(ctx, env.ev.ID)
		got, _ := env.store.GetSession(ctx, a.ID)
		if stored.AttendanceMinCount != nil || !got.CertificateCounted {
			t.Fatalf("%s wrote: %+v %+v", name, stored, got)
		}
	}
	// Who may not edit the Event may not write its rule.
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if _, err := env.svc.SetCertificateRule(ctx, member, env.ev.ID, event.CertificateRuleInput{Rule: "once"}); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	otherLeader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/AIRLAB/LIDERLER"}}
	if _, err := env.svc.SetCertificateRule(ctx, otherLeader, env.ev.ID, event.CertificateRuleInput{Rule: "once"}); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("another team's leader: %v", err)
	}
	if _, err := env.svc.CertificateRule(ctx, otherLeader, env.ev.ID); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("another team's leader reads: %v", err)
	}
}

// An Event on the ratio rule keeps it through writes that do not touch the
// rule, and through one that sends it back unchanged; a new ratio, or the
// ratio rule on another Event, is refused.
func TestRatioRuleKeptOnExistingEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, svc := setup(t)
	r := 0.75
	ratioEvent, err := store.Create(ctx, event.Event{Name: "ARTLAB 2025", Location: "YTÜ", OwnerTeam: "WEBLAB", AttendanceRule: "ratio", AttendanceRatio: &r})
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := svc.Patch(ctx, copyLeader, ratioEvent.ID, event.Patch{Name: optional.Field[string]{Set: true, Value: "ARTLAB 2025 Bahar"}})
	if err != nil || renamed.AttendanceRule != "ratio" || renamed.AttendanceRatio == nil || *renamed.AttendanceRatio != 0.75 {
		t.Fatalf("renamed %+v %v", renamed, err)
	}
	same := 0.75
	if _, err := svc.Patch(ctx, copyLeader, ratioEvent.ID, event.Patch{
		AttendanceRule:  optional.Field[string]{Set: true, Value: "ratio"},
		AttendanceRatio: optional.Field[*float64]{Set: true, Value: &same},
	}); err != nil {
		t.Fatalf("sent back unchanged: %v", err)
	}
	other := 0.6
	if _, err := svc.Patch(ctx, copyLeader, ratioEvent.ID, event.Patch{AttendanceRatio: optional.Field[*float64]{Set: true, Value: &other}}); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("a new ratio: %v", err)
	}
	countEvent, err := svc.Create(ctx, copyLeader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", AttendanceRule: "count"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Patch(ctx, copyLeader, countEvent.ID, event.Patch{
		AttendanceRule:  optional.Field[string]{Set: true, Value: "ratio"},
		AttendanceRatio: optional.Field[*float64]{Set: true, Value: &same},
	}); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("ratio on a count Event: %v", err)
	}
	// Turning the ratio Event to count is a new write: the ratio goes.
	counted, err := svc.Patch(ctx, copyLeader, ratioEvent.ID, event.Patch{AttendanceRule: optional.Field[string]{Set: true, Value: "count"}})
	if err != nil || counted.AttendanceRule != "count" || counted.AttendanceRatio != nil || counted.AttendanceMinCount != nil {
		t.Fatalf("to count %+v %v", counted, err)
	}
	stored, _ := store.Get(ctx, ratioEvent.ID)
	if rule, err := svc.CertificateRule(ctx, copyLeader, stored.ID); err != nil || rule.Rule != "count" {
		t.Fatalf("rule %+v %v", rule, err)
	}
}

// The ratio rule's effective threshold is the fewest check-ins its float
// comparison takes: 6 of 8 at 0.75.
func TestCertificateRuleReadsTheRatioThreshold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRuleEnv(t)
	for _, title := range []string{"1", "2", "3", "4", "5", "6", "7", "8"} {
		env.session(t, title, "TALK", event.SessionCertificate{})
	}
	r := 0.75
	stored, err := env.store.Get(ctx, env.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.AttendanceRule, stored.AttendanceRatio = "ratio", &r
	if _, err := env.store.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	rule, err := env.svc.CertificateRule(ctx, copyLeader, env.ev.ID)
	if err != nil || rule.Rule != "ratio" || rule.Ratio == nil || *rule.Ratio != 0.75 || rule.EffectiveMin != 6 || rule.MinCount != nil {
		t.Fatalf("rule %+v %v", rule, err)
	}
}

// A copy carries the threshold and every Session's marks.
func TestCopyCarriesTheCertificateRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRuleEnv(t)
	counted := env.session(t, "Açılış", "TALK", event.SessionCertificate{Required: mark(true)})
	env.session(t, "Tanışma", "NETWORKING", event.SessionCertificate{})
	six := 6
	if _, err := env.svc.SetCertificateRule(ctx, copyLeader, env.ev.ID, event.CertificateRuleInput{
		Rule: "count", MinCount: &six, Sessions: []event.SessionCertificateFlags{{ID: counted.ID, Counted: true, Required: true}},
	}); err != nil {
		t.Fatal(err)
	}
	copied, _, err := env.svc.Copy(ctx, copyLeader, env.ev.ID, event.CopyInput{ID: uuid.New(), Name: "ARTLAB 2027", StartDate: stamp("2027-03-14T19:00:00+03:00")})
	if err != nil {
		t.Fatal(err)
	}
	if copied.Event.AttendanceRule != "count" || copied.Event.AttendanceMinCount == nil || *copied.Event.AttendanceMinCount != 6 {
		t.Fatalf("copy %+v", copied.Event)
	}
	rule, err := env.svc.CertificateRule(ctx, copyLeader, copied.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	marks := map[string][2]bool{}
	for _, s := range rule.Sessions {
		marks[s.Title] = [2]bool{s.Counted, s.Required}
	}
	if marks["Açılış"] != [2]bool{true, true} || marks["Tanışma"] != [2]bool{false, false} || rule.CountedTotal != 1 {
		t.Fatalf("copied marks %v %+v", marks, rule)
	}
}

// An Event with no live Session reads sessionless: the Event itself is the
// one counted unit (N 1, threshold 1) and a Venue entry its attendance; a
// live Session turns it back (docs/certificate-rule.md).
func TestCertificateRuleSessionless(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newRuleEnv(t)
	rule, err := env.svc.CertificateRule(ctx, copyLeader, env.ev.ID)
	if err != nil || !rule.Sessionless || rule.CountedTotal != 1 || rule.EffectiveMin != 1 {
		t.Fatalf("no Session: %+v %v", rule, err)
	}
	env.session(t, "Açılış", "TALK", event.SessionCertificate{})
	rule, err = env.svc.CertificateRule(ctx, copyLeader, env.ev.ID)
	if err != nil || rule.Sessionless || rule.CountedTotal != 1 {
		t.Fatalf("one Session: %+v %v", rule, err)
	}
}
