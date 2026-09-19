package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

func TestServiceArchivesAndRestoresEventIdempotently(t *testing.T) {
	t.Parallel()
	store, svc := setup(t)
	ctx := context.Background()
	leaderID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	leader := authz.Principal{ID: leaderID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	stranger := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/SKYSEC/LIDERLER"}}

	created, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, leader, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, leader, created.ID); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	if _, err := svc.Get(ctx, created.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("archived event Get error = %v", err)
	}

	archived, err := store.GetIncludingArchived(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.ArchivedAt == nil || archived.ArchivedBy == nil || *archived.ArchivedBy != leaderID {
		t.Fatalf("archive metadata = %+v", archived)
	}
	visible, err := svc.ListLifecycle(ctx, leader, "", lifecycle.InactiveOnly)
	if err != nil || len(visible) != 1 || visible[0].ID != created.ID {
		t.Fatalf("leader archive list = %+v, err = %v", visible, err)
	}
	visible, err = svc.ListLifecycle(ctx, stranger, "", lifecycle.InactiveOnly)
	if err != nil || len(visible) != 0 {
		t.Fatalf("stranger archive list = %+v, err = %v", visible, err)
	}

	restored, err := svc.Restore(ctx, leader, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ArchivedAt != nil || restored.ArchivedBy != nil {
		t.Fatalf("restored event = %+v", restored)
	}
	if _, err := svc.Restore(ctx, leader, created.ID); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if _, err := svc.Get(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRestoresScheduleOnlyUnderCurrentAncestors(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{
		ID: uuid.MustParse("22222222-2222-2222-2222-222222222222").String(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"},
	}

	ev, err := svc.Create(ctx, leader, event.Event{Name: "Program", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	day, err := svc.CreateDay(ctx, leader, event.Day{EventID: ev.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.CreateSession(ctx, leader, event.Session{
		EventDayID: day.ID, Title: "Talk", SpeakerName: "Ada", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteSession(ctx, leader, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSession(ctx, leader, session.ID); err != nil {
		t.Fatalf("second session archive: %v", err)
	}
	if err := svc.DeleteDay(ctx, leader, day.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestoreSession(ctx, leader, session.ID); !errors.Is(err, event.ErrConflict) {
		t.Fatalf("restore under archived day = %v", err)
	}
	if _, err := svc.RestoreDay(ctx, leader, day.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestoreSession(ctx, leader, session.ID); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteDay(ctx, leader, day.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, leader, ev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestoreDay(ctx, leader, day.ID); !errors.Is(err, event.ErrConflict) {
		t.Fatalf("restore under archived event = %v", err)
	}
	if _, err := svc.Restore(ctx, leader, ev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestoreDay(ctx, leader, day.ID); err != nil {
		t.Fatal(err)
	}
}
