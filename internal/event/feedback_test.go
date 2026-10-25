package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// The Event's feedback form (etkinlik geri bildirim formu, ticket 02).

func formURL(id uuid.UUID) string {
	return "https://forms.yildizskylab.com/" + id.String()
}

// The feedback form's short link is the Event's to manage, labelled as
// feedback; the form is not listed (its answers write no Ticket).
func TestFeedbackFormFieldsAndLink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	links := &recordedSync{}
	svc := event.NewServiceWithOptions(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{FormLinks: links})
	apply, feedback := uuid.New(), uuid.New()
	created, err := svc.Create(ctx, copyLeader, event.Event{
		Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB",
		FormURL: formURL(apply), FormAlias: "gecekodu",
		FeedbackFormURL: formURL(feedback), FeedbackFormAlias: "gecekodu-geri-bildirim", FeedbackAutoSend: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.FeedbackMode != event.FeedbackEvent || created.FeedbackFormURL != formURL(feedback) || !created.FeedbackAutoSend {
		t.Fatalf("created %+v", created)
	}
	if !created.ListsForm(apply) || created.ListsForm(feedback) || !created.HasFeedbackForm(feedback) {
		t.Fatal("ListsForm takes in the feedback form")
	}
	saved := links.last(t, 1)
	if len(saved) != 2 || saved[1].FormID != feedback || saved[1].Alias != "gecekodu-geri-bildirim" || saved[1].Label != "Gecekodu · Geri bildirim" {
		t.Fatalf("links %+v", saved)
	}

	// A body without the feedback fields keeps them, and does not sync.
	updated, err := svc.Patch(ctx, copyLeader, created.ID, patchOf(t, `{"description":"Yeni"}`))
	if err != nil || updated.FeedbackFormURL != formURL(feedback) || updated.FeedbackFormAlias != "gecekodu-geri-bildirim" ||
		updated.FeedbackMode != event.FeedbackEvent || !updated.FeedbackAutoSend {
		t.Fatalf("kept %+v %v", updated, err)
	}
	links.last(t, 1)
	updated, err = svc.Patch(ctx, copyLeader, created.ID, patchOf(t, `{"feedbackMode":"per_session","feedbackAutoSend":false}`))
	if err != nil || updated.FeedbackMode != event.FeedbackPerSession || updated.FeedbackAutoSend || updated.FeedbackFormURL == "" {
		t.Fatalf("mode %+v %v", updated, err)
	}
	for _, raw := range []string{`{"feedbackMode":"everyone"}`, `{"feedbackMode":null}`, `{"feedbackAutoSend":null}`} {
		if _, err := svc.Patch(ctx, copyLeader, created.ID, patchOf(t, raw)); !errors.Is(err, event.ErrInvalid) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	// Clearing the form releases its link.
	updated, err = svc.Patch(ctx, copyLeader, created.ID, patchOf(t, `{"feedbackFormUrl":null,"feedbackFormAlias":""}`))
	if err != nil || updated.FeedbackFormURL != "" || updated.FeedbackFormAlias != "" {
		t.Fatalf("cleared %+v %v", updated, err)
	}
	if saved := links.last(t, 2); len(saved) != 1 || saved[0].FormID != apply {
		t.Fatalf("links after clearing %+v", saved)
	}
	// Archiving releases every link, the feedback form's too.
	if err := svc.Delete(ctx, copyYK, created.ID); err != nil {
		t.Fatal(err)
	}
	if saved := links.last(t, 3); saved != nil {
		t.Fatalf("archived: %+v", saved)
	}
	if _, err := svc.Create(ctx, copyLeader, event.Event{Name: "x", Location: "YTÜ", OwnerTeam: "WEBLAB", FeedbackMode: "everyone"}); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("create with an unknown mode: %v", err)
	}
}

// A feedback form is in one place only: not also an application or extra
// form, nor another current Event's form. Application forms stay shareable.
func TestFeedbackFormOnePlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := event.NewService(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	shared, feedback, extra := uuid.New(), uuid.New(), uuid.New()
	first, err := svc.Create(ctx, copyLeader, event.Event{
		Name: "Birinci", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: formURL(shared),
		ExtraFormURLs:   []event.EventFormLink{{Label: "CTF", URL: formURL(extra)}},
		FeedbackFormURL: formURL(feedback),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		in   event.Event
	}{
		{"its own application form", event.Event{FormURL: formURL(shared), FeedbackFormURL: formURL(shared)}},
		{"its own extra form", event.Event{ExtraFormURLs: []event.EventFormLink{{Label: "Anket", URL: formURL(extra)}}, FeedbackFormURL: formURL(extra) + "?utm_source=x"}},
		{"another Event's feedback form", event.Event{FeedbackFormURL: formURL(feedback)}},
		{"another Event's application form", event.Event{FeedbackFormURL: formURL(shared)}},
		{"another Event's extra form", event.Event{FeedbackFormURL: formURL(extra)}},
		{"another Event's feedback form as an application form", event.Event{FormURL: formURL(feedback)}},
		{"another Event's feedback form as an extra form", event.Event{ExtraFormURLs: []event.EventFormLink{{Label: "x", URL: formURL(feedback)}}}},
	} {
		tc.in.Name, tc.in.Location, tc.in.OwnerTeam = "İkinci", "YTÜ", "WEBLAB"
		if _, err := svc.Create(ctx, copyLeader, tc.in); !errors.Is(err, event.ErrFormInUse) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// The application form is still shared, as before.
	second, err := svc.Create(ctx, copyLeader, event.Event{Name: "İkinci", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: formURL(shared)})
	if err != nil {
		t.Fatal(err)
	}
	// An update is held to it too.
	if _, err := svc.Patch(ctx, copyLeader, second.ID, patchOf(t, `{"feedbackFormUrl":"`+formURL(feedback)+`"}`)); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("update: %v", err)
	}
	// An archived Event released its forms; restoring it while its feedback
	// form is somewhere else is refused.
	if err := svc.Delete(ctx, copyYK, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Patch(ctx, copyLeader, second.ID, patchOf(t, `{"feedbackFormUrl":"`+formURL(feedback)+`"}`)); err != nil {
		t.Fatalf("after archive: %v", err)
	}
	if _, err := svc.Restore(ctx, copyYK, first.ID); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("restore: %v", err)
	}
}

// A copy keeps the feedback mode and the "send when it ends" choice, not
// the form (one place).
func TestCopyKeepsFeedbackModeNotForm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	source, err := svc.Create(ctx, copyLeader, event.Event{
		Name: "Kaynak", Location: "YTÜ", OwnerTeam: "WEBLAB", FeedbackMode: event.FeedbackEventWithSessions,
		FeedbackFormURL: formURL(uuid.New()), FeedbackFormAlias: "kaynak-geri-bildirim", FeedbackAutoSend: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := svc.Copy(ctx, copyLeader, source.ID, copyIn("Kopya", "2027-01-01T10:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	c := got.Event
	if c.FeedbackMode != event.FeedbackEventWithSessions || c.FeedbackFormURL != "" || c.FeedbackFormAlias != "" || c.FeedbackAutoSend {
		t.Fatalf("copy %+v", c)
	}
	// A source mailed automatically gives a copy mailed automatically.
	on, err := svc.Create(ctx, copyLeader, event.Event{Name: "Açık", Location: "YTÜ", OwnerTeam: "WEBLAB", FeedbackAutoSend: true})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err = svc.Copy(ctx, copyLeader, on.ID, copyIn("Kopya 2", "2027-01-01T10:00:00+03:00"))
	if err != nil || !got.Event.FeedbackAutoSend {
		t.Fatalf("copy of auto send %+v %v", got.Event, err)
	}
}
