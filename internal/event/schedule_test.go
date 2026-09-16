package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

func TestService_DayAndSessionOwnerRules(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	ctx := context.Background()
	leader := authz.Principal{ID: "l", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	stranger := authz.Principal{ID: "s", Groups: []string{"/UYELER/ARGE/SKYSEC/LIDERLER"}}

	ev, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	day, err := svc.CreateDay(ctx, leader, event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	days, err := svc.ListDays(ctx, ev.ID)
	if err != nil || len(days) != 1 {
		t.Fatalf("days %+v %v", days, err)
	}
	if _, err := svc.CreateDay(ctx, stranger, event.Day{EventID: ev.ID, Name: "Nope"}); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("stranger day: %v", err)
	}

	sess, err := svc.CreateSession(ctx, leader, event.Session{
		EventDayID: day.ID, Title: "Talk", SpeakerName: "Ada", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := svc.ListSessions(ctx, day.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != sess.ID {
		t.Fatalf("sessions %+v %v", listed, err)
	}
	if err := svc.DeleteSession(ctx, stranger, sess.ID); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("stranger delete session: %v", err)
	}
	if err := svc.DeleteSession(ctx, leader, sess.ID); err != nil {
		t.Fatal(err)
	}
}
