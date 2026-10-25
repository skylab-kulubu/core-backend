package event_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The store writes and reads the feedback fields; an update writes them
// as given; a copy takes the mode and the "send when it ends" choice, not
// the form.
func TestPostgresStoreFeedbackFields(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	form := formURL(uuid.New())
	created, err := store.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", FeedbackMode: event.FeedbackPerSession,
		FeedbackFormURL: form, FeedbackFormAlias: "hack-gb", FeedbackAutoSend: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.FeedbackMode != event.FeedbackPerSession || created.FeedbackFormURL != form || created.FeedbackFormAlias != "hack-gb" || !created.FeedbackAutoSend {
		t.Fatalf("created %+v", created)
	}
	created.FeedbackMode, created.FeedbackFormURL, created.FeedbackFormAlias, created.FeedbackAutoSend = event.FeedbackEventWithSessions, "", "", false
	updated, err := store.Update(ctx, created)
	if err != nil || updated.FeedbackMode != event.FeedbackEventWithSessions || updated.FeedbackFormURL != "" || updated.FeedbackAutoSend {
		t.Fatalf("updated %+v %v", updated, err)
	}
	// An Event created with no mode gets the default.
	plain, err := store.Create(ctx, event.Event{Name: "Düz", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil || plain.FeedbackMode != event.FeedbackEvent {
		t.Fatalf("plain %+v %v", plain.FeedbackMode, err)
	}
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	copied, _, err := svc.Copy(ctx, copyLeader, updated.ID, copyIn("Kopya", "2027-01-01T10:00:00+03:00"))
	if err != nil || copied.Event.FeedbackMode != event.FeedbackEventWithSessions || copied.Event.FeedbackFormURL != "" || copied.Event.FeedbackAutoSend {
		t.Fatalf("copy %+v %v", copied.Event, err)
	}
	mailed, err := store.Create(ctx, event.Event{Name: "Postalı", Location: "YTÜ", OwnerTeam: "WEBLAB", FeedbackAutoSend: true})
	if err != nil {
		t.Fatal(err)
	}
	copied, _, err = svc.Copy(ctx, copyLeader, mailed.ID, copyIn("Kopya 2", "2027-01-01T10:00:00+03:00"))
	if err != nil || !copied.Event.FeedbackAutoSend {
		t.Fatalf("copy of auto send %+v %v", copied.Event, err)
	}
}
