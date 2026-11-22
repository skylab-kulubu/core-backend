package event_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// An application or extra form is on one current Event only, a draft or an
// ended one too (ADR 0069, addendum of 2026-11-22): a second binding is
// 409 form_in_use, naming the Event that holds it to who may see it.

func formHolder(t *testing.T, err error) *event.FormHolder {
	t.Helper()
	var inUse *event.FormInUseError
	if !errors.As(err, &inUse) || !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("want form_in_use, got %v", err)
	}
	return inUse.Holder
}

func TestApplicationFormOnOneEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := event.NewService(event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	past := time.Now().Add(-30 * 24 * time.Hour)
	apply, extra, draftForm, endedForm := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	first, err := svc.Create(ctx, copyLeader, event.Event{
		Name: "Birinci", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusPublished,
		FormURL: formURL(apply), ExtraFormURLs: []event.EventFormLink{{Label: "CTF", URL: formURL(extra)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := svc.Create(ctx, copyLeader, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, FormURL: formURL(draftForm)})
	if err != nil {
		t.Fatal(err)
	}
	ended, err := svc.Create(ctx, copyLeader, event.Event{Name: "Geçen yıl", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusPublished, StartDate: &past, FormURL: formURL(endedForm)})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		in     event.Event
		holder uuid.UUID
	}{
		{"another Event's application form", event.Event{FormURL: formURL(apply)}, first.ID},
		{"another Event's extra form as the application form", event.Event{FormURL: formURL(extra) + "?utm_source=x"}, first.ID},
		{"another Event's application form as an extra form", event.Event{ExtraFormURLs: []event.EventFormLink{{Label: "x", URL: formURL(apply)}}}, first.ID},
		{"a draft's form", event.Event{FormURL: formURL(draftForm)}, draft.ID},
		{"an ended Event's form", event.Event{FormURL: formURL(endedForm)}, ended.ID},
	} {
		tc.in.Name, tc.in.Location, tc.in.OwnerTeam = "İkinci", "YTÜ", "WEBLAB"
		holder := formHolder(t, func() error { _, err := svc.Create(ctx, copyLeader, tc.in); return err }())
		if holder == nil || holder.ID != tc.holder {
			t.Errorf("%s: holder %+v, want %s", tc.name, holder, tc.holder)
		}
	}
	if holder := formHolder(t, func() error {
		_, err := svc.Create(ctx, copyLeader, event.Event{Name: "İkinci", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: formURL(apply)})
		return err
	}()); holder.Name != "Birinci" || holder.Status != event.StatusPublished {
		t.Fatalf("holder %+v", holder)
	}

	// Another team's leader is told a published Event holds it, but not of
	// a draft they may not see.
	if holder := formHolder(t, func() error {
		_, err := svc.Create(ctx, copyOther, event.Event{Name: "Art", Location: "YTÜ", OwnerTeam: "ARTLAB", FormURL: formURL(apply)})
		return err
	}()); holder == nil || holder.ID != first.ID {
		t.Fatalf("published holder for another team: %+v", holder)
	}
	if holder := formHolder(t, func() error {
		_, err := svc.Create(ctx, copyOther, event.Event{Name: "Art", Location: "YTÜ", OwnerTeam: "ARTLAB", FormURL: formURL(draftForm)})
		return err
	}()); holder != nil {
		t.Fatalf("a draft another team may not see is named: %+v", holder)
	}

	// An update is held to it too, a PATCH and a PUT.
	second, err := svc.Create(ctx, copyLeader, event.Event{Name: "İkinci", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Patch(ctx, copyLeader, second.ID, patchOf(t, `{"formUrl":"`+formURL(apply)+`"}`)); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("patch: %v", err)
	}
	if _, err := svc.Update(ctx, copyLeader, second.ID, event.Event{Name: "İkinci", Location: "YTÜ", OwnerTeam: "WEBLAB", ExtraFormURLs: []event.EventFormLink{{Label: "x", URL: formURL(extra)}}}); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("put: %v", err)
	}
	// The Event that holds it writes it again, and may move it between its
	// own slots.
	if _, err := svc.Patch(ctx, copyLeader, first.ID, patchOf(t, `{"formUrl":"`+formURL(extra)+`","extraFormUrls":[{"label":"Ana","url":"`+formURL(apply)+`"}]}`)); err != nil {
		t.Fatalf("own forms: %v", err)
	}

	// An archived Event holds none: the form is free, and restoring the
	// archived Event while it is taken is refused.
	if err := svc.Delete(ctx, copyLeader, draft.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Patch(ctx, copyLeader, second.ID, patchOf(t, `{"formUrl":"`+formURL(draftForm)+`"}`)); err != nil {
		t.Fatalf("after archive: %v", err)
	}
	if _, err := svc.Restore(ctx, copyLeader, draft.ID); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("restore: %v", err)
	}
}

// A form two current Events listed before the rule stays on both: their
// writes go through as before (a PUT sends the form back), only a third
// binding is refused (ADR 0069, addendum of 2026-11-22: no data changes).
func TestApplicationFormSharedBeforeTheRuleIsKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	shared := uuid.New()
	var legacy []event.Event
	for _, name := range []string{"Eski", "Yeni"} {
		e, err := store.Create(ctx, event.Event{Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusPublished, FormURL: formURL(shared)})
		if err != nil {
			t.Fatal(err)
		}
		legacy = append(legacy, e)
	}
	for _, e := range legacy {
		if _, err := svc.Update(ctx, copyLeader, e.ID, event.Event{Name: e.Name, Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: formURL(shared), Description: "güncel"}); err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		if _, err := svc.Patch(ctx, copyLeader, e.ID, patchOf(t, `{"formUrl":"`+formURL(shared)+`","name":"`+e.Name+` 2"}`)); err != nil {
			t.Fatalf("%s patch: %v", e.Name, err)
		}
	}
	if _, err := svc.Create(ctx, copyLeader, event.Event{Name: "Üçüncü", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: formURL(shared)}); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("third: %v", err)
	}
	// Removed from one, it is not taken back by it.
	if _, err := svc.Patch(ctx, copyLeader, legacy[0].ID, patchOf(t, `{"formUrl":""}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Patch(ctx, copyLeader, legacy[0].ID, patchOf(t, `{"formUrl":"`+formURL(shared)+`"}`)); !errors.Is(err, event.ErrFormInUse) {
		t.Fatalf("bound again: %v", err)
	}
}
