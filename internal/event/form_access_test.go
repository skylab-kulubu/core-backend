package event_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// stubFormAccess answers each form's check: a form in refused gets its
// error, any other passes. asked records the forms checked, in order.
type stubFormAccess struct {
	refused map[uuid.UUID]error
	asked   []uuid.UUID
}

func (s *stubFormAccess) SeesForm(_ context.Context, _ authz.Principal, formID uuid.UUID) error {
	s.asked = append(s.asked, formID)
	return s.refused[formID]
}

// An Event lists a form newly only when the person writing it is on the
// form's team in Forms (docs/form-respondents.md#who-may-link-a-form): a
// refusal writes nothing, binds no short link and queues no gathering; a
// form the Event lists already is not asked about.
func TestFormAccessBeforeLinking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mine, viewed, theirs, down := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	access := &stubFormAccess{refused: map[uuid.UUID]error{theirs: event.ErrFormsAccessDenied, down: event.ErrFormsUnavailable}}
	linked := &linkedForms{}
	links := &recordedSync{}
	store := event.NewMemoryStore()
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{
		FormAccess: access, FormsLinked: linked, FormLinks: links,
	})
	stored := func() []event.Event {
		t.Helper()
		all, err := store.ListIncludingDrafts(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		return all
	}

	// Create: another's form, and a Forms that cannot tell, write nothing.
	for _, tc := range []struct {
		name string
		in   event.Event
		want error
	}{
		{"another's application form", event.Event{FormURL: formURL(theirs)}, event.ErrFormsAccessDenied},
		{"another's extra form beside one's own", event.Event{FormURL: formURL(mine), ExtraFormURLs: []event.EventFormLink{{Label: "CTF", URL: formURL(theirs)}}}, event.ErrFormsAccessDenied},
		{"Forms down", event.Event{FormURL: formURL(down)}, event.ErrFormsUnavailable},
	} {
		tc.in.Name, tc.in.Location, tc.in.OwnerTeam = "Gecekodu", "YTÜ", "WEBLAB"
		if _, err := svc.Create(ctx, copyLeader, tc.in); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if n := len(stored()); n != 0 || len(linked.calls) != 0 || len(links.calls) != 0 {
		t.Fatalf("a refused create wrote: events %d, gatherings %v, links %d", n, linked.calls, len(links.calls))
	}

	// Its own form and one it views in Forms are linked, each asked once.
	access.asked = nil
	created, err := svc.Create(ctx, copyLeader, event.Event{
		Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB",
		FormURL: formURL(mine), ExtraFormURLs: []event.EventFormLink{{Label: "CTF", URL: formURL(viewed)}, {Label: "Yine", URL: formURL(mine) + "?ref=x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(access.asked, []uuid.UUID{mine, viewed}) || len(linked.calls) != 1 {
		t.Fatalf("asked %v, gatherings %v", access.asked, linked.calls)
	}

	// Update: adding another's form is refused and the stored Event stays
	// as it was; the forms it keeps are not asked about.
	before := len(links.calls)
	access.asked = nil
	if _, err := svc.Patch(ctx, copyLeader, created.ID, patchOf(t, `{"extraFormUrls":[{"label":"CTF","url":"`+formURL(viewed)+`"},{"label":"Başka","url":"`+formURL(theirs)+`"}]}`)); !errors.Is(err, event.ErrFormsAccessDenied) {
		t.Fatalf("patch another's form: %v", err)
	}
	if !slices.Equal(access.asked, []uuid.UUID{theirs}) {
		t.Fatalf("patch asked %v, want only the new form", access.asked)
	}
	if _, err := svc.Update(ctx, copyLeader, created.ID, event.Event{Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: formURL(theirs)}); !errors.Is(err, event.ErrFormsAccessDenied) {
		t.Fatalf("put another's form: %v", err)
	}
	if _, err := svc.Patch(ctx, copyLeader, created.ID, patchOf(t, `{"formUrl":"`+formURL(down)+`"}`)); !errors.Is(err, event.ErrFormsUnavailable) {
		t.Fatalf("patch, Forms down: %v", err)
	}
	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FormURL != formURL(mine) || len(got.ExtraFormURLs) != 2 || got.ExtraFormURLs[0].URL != formURL(viewed) {
		t.Fatalf("a refused update wrote: %+v %+v", got.FormURL, got.ExtraFormURLs)
	}
	if len(linked.calls) != 1 || len(links.calls) != before {
		t.Fatalf("a refused update linked: gatherings %v, links %d→%d", linked.calls, before, len(links.calls))
	}

	// A write that keeps the forms reads no Forms: the name, the forms sent
	// again (a PUT, another address of the same form), moved between slots.
	access.asked = nil
	if _, err := svc.Patch(ctx, copyLeader, created.ID, patchOf(t, `{"name":"Gecekodu 2"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Patch(ctx, copyLeader, created.ID, patchOf(t, `{"formUrl":"`+formURL(viewed)+`?utm_source=x","extraFormUrls":[{"label":"Ana","url":"`+formURL(mine)+`"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, copyLeader, created.ID, event.Event{Name: "Gecekodu 2", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: formURL(viewed)}); err != nil {
		t.Fatal(err)
	}
	if len(access.asked) != 0 {
		t.Fatalf("kept forms asked about: %v", access.asked)
	}

	// A link made before the check stays: the forms it keeps are never
	// asked about, even ones the person could not link today.
	legacy, err := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{}).
		Create(ctx, copyLeader, event.Event{Name: "Eski", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: formURL(theirs)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Patch(ctx, copyLeader, legacy.ID, patchOf(t, `{"name":"Eski 2","formUrl":"`+formURL(theirs)+`"}`)); err != nil {
		t.Fatalf("an old link kept: %v", err)
	}
	if len(access.asked) != 0 {
		t.Fatalf("an old link asked about: %v", access.asked)
	}
}
