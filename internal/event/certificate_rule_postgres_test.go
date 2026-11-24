package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// On PostgreSQL: the rule and the marks are written in one transaction (a
// refused Session leaves everything as it was), and a copy carries them.
func TestPostgresCertificateRule(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	newEvent := func(name string) (event.Event, event.Day) {
		t.Helper()
		ev, err := svc.Create(ctx, copyLeader, event.Event{Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB", AttendanceRule: "count"})
		if err != nil {
			t.Fatal(err)
		}
		day, err := svc.CreateDay(ctx, copyLeader, event.Day{EventID: ev.ID, Name: "Cumartesi"})
		if err != nil {
			t.Fatal(err)
		}
		return ev, day
	}
	ev, day := newEvent("ARTLAB")
	session := func(day event.Day, title, sessionType string) event.Session {
		t.Helper()
		sess, err := svc.CreateSession(ctx, copyLeader, event.Session{EventDayID: day.ID, Title: title, SpeakerName: "Ada", SessionType: sessionType})
		if err != nil {
			t.Fatal(err)
		}
		return sess
	}
	a, b := session(day, "Açılış", "TALK"), session(day, "Final", "TALK")
	networking := session(day, "Tanışma", "NETWORKING")
	if !a.CertificateCounted || a.CertificateRequired || networking.CertificateCounted {
		t.Fatalf("defaults %+v %+v", a, networking)
	}
	_, otherDay := newEvent("Başka")
	foreign := session(otherDay, "Başka", "TALK")

	six := 6
	if _, err := svc.SetCertificateRule(ctx, copyLeader, ev.ID, event.CertificateRuleInput{
		Rule: "count", MinCount: &six,
		Sessions: []event.SessionCertificateFlags{{ID: a.ID, Counted: false}, {ID: foreign.ID, Counted: true}},
	}); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("another Event's Session: %v", err)
	}
	stored, err := store.Get(ctx, ev.ID)
	if err != nil || stored.AttendanceMinCount != nil {
		t.Fatalf("a refused write wrote the threshold: %+v %v", stored, err)
	}
	if got, _ := store.GetSession(ctx, a.ID); !got.CertificateCounted {
		t.Fatalf("a refused write wrote a mark: %+v", got)
	}
	if got, _ := store.GetSession(ctx, foreign.ID); !got.CertificateCounted {
		t.Fatalf("another Event's Session changed: %+v", got)
	}

	if stored.AttendanceAutoFinalize {
		t.Fatal("a refused write turned the switch on")
	}
	on := true
	rule, err := svc.SetCertificateRule(ctx, copyLeader, ev.ID, event.CertificateRuleInput{
		Rule: "count", MinCount: &six, AutoFinalize: &on,
		Sessions: []event.SessionCertificateFlags{{ID: b.ID, Counted: true, Required: true}, {ID: networking.ID, Counted: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule.MinCount == nil || *rule.MinCount != 6 || rule.CountedTotal != 3 || rule.EffectiveMin != 3 || !rule.AutoFinalize {
		t.Fatalf("rule %+v", rule)
	}
	// A write that leaves the switch out keeps it.
	if kept, err := svc.SetCertificateRule(ctx, copyLeader, ev.ID, event.CertificateRuleInput{Rule: "count", MinCount: &six}); err != nil || !kept.AutoFinalize {
		t.Fatalf("kept %v %+v", err, kept)
	}
	if stored, err := store.Get(ctx, ev.ID); err != nil || !stored.AttendanceAutoFinalize {
		t.Fatalf("stored switch %v %+v", err, stored.AttendanceAutoFinalize)
	}
	if got, _ := store.GetSession(ctx, b.ID); !got.CertificateRequired || !got.CertificateCounted {
		t.Fatalf("final %+v", got)
	}

	copied, _, err := svc.Copy(ctx, copyLeader, ev.ID, event.CopyInput{ID: uuid.New(), Name: "ARTLAB 2027", StartDate: stamp("2027-03-14T19:00:00+03:00")})
	if err != nil {
		t.Fatal(err)
	}
	if copied.Event.AttendanceRule != "count" || copied.Event.AttendanceMinCount == nil || *copied.Event.AttendanceMinCount != 6 {
		t.Fatalf("copy %+v", copied.Event)
	}
	copiedRule, err := svc.CertificateRule(ctx, copyLeader, copied.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !copiedRule.AutoFinalize {
		t.Fatal("the copy's attendance does not close by itself")
	}
	marks := map[string][2]bool{}
	for _, s := range copiedRule.Sessions {
		marks[s.Title] = [2]bool{s.Counted, s.Required}
	}
	if marks["Açılış"] != [2]bool{true, false} || marks["Final"] != [2]bool{true, true} || marks["Tanışma"] != [2]bool{true, false} {
		t.Fatalf("copied marks %v", marks)
	}
}
