package formsfeed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

type backfillEvents struct{ events []event.Event }

func (b backfillEvents) Get(_ context.Context, id uuid.UUID) (event.Event, error) {
	for _, ev := range b.events {
		if ev.ID == id && ev.ArchivedAt == nil {
			return ev, nil
		}
	}
	return event.Event{}, event.ErrNotFound
}

func (b backfillEvents) ListIncludingDrafts(context.Context, string) ([]event.Event, error) {
	var out []event.Event
	for _, ev := range b.events {
		if ev.ArchivedAt == nil {
			out = append(out, ev)
		}
	}
	return out, nil
}

func TestBackfillRequestRules(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(72*time.Hour), now.Add(-72*time.Hour)
	form, shared := uuid.New(), uuid.New()
	address := func(id uuid.UUID) string { return "https://forms.yildizskylab.com/" + id.String() }
	open := event.Event{ID: uuid.New(), OwnerTeam: "WEBLAB", Status: event.StatusPublished, FormURL: address(form), StartDate: &future}
	draft := event.Event{ID: uuid.New(), OwnerTeam: "WEBLAB", Status: event.StatusDraft, FormURL: address(uuid.New())}
	cancelled := event.Event{ID: uuid.New(), OwnerTeam: "WEBLAB", Status: event.StatusCancelled, FormURL: address(uuid.New())}
	ended := event.Event{ID: uuid.New(), OwnerTeam: "WEBLAB", Status: event.StatusPublished, FormURL: address(uuid.New()), StartDate: &past, EndDate: &past}
	sharedA := event.Event{ID: uuid.New(), OwnerTeam: "WEBLAB", Status: event.StatusPublished, FormURL: address(shared)}
	sharedB := event.Event{ID: uuid.New(), OwnerTeam: "GAMELAB", Status: event.StatusPublished, FeedbackFormURL: address(shared)}
	editor := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/GAMELAB"}}
	service := authz.Principal{ID: uuid.NewString(), ServiceAccount: true, Groups: []string{"/UYELER/YK"}}
	// The editor's place on the form's team in Forms: a viewer is enough
	// (Yusuf, 2026-11-26).
	teamForm := func(role string) FormReader {
		return stubForm{form: Form{FormID: form, AccessListed: true, Collaborators: []Collaborator{
			{UserID: uuid.MustParse(editor.ID), Role: role}, {UserID: uuid.New(), Role: RoleOwner}}}}
	}
	viewer := teamForm(RoleViewer)

	cases := []struct {
		name    string
		p       authz.Principal
		ev      uuid.UUID
		form    uuid.UUID
		on      bool
		forms   FormReader
		noForms bool
		want    error
		again   bool
		after   time.Duration
	}{
		{name: "queued", p: editor, ev: open.ID, form: form, on: true},
		{name: "asked again within 15 minutes", p: editor, ev: open.ID, form: form, on: true, again: true, after: 14 * time.Minute, want: ErrBackfillCooldown},
		{name: "waiting already", p: editor, ev: open.ID, form: form, on: true, again: true, after: 16 * time.Minute},
		{name: "no such Event", p: editor, ev: uuid.New(), form: form, on: true, want: ErrBackfillEventNotFound},
		{name: "may not edit", p: member, ev: open.ID, form: form, on: true, want: ErrBackfillForbidden},
		{name: "a draft hidden", p: member, ev: draft.ID, form: form, on: true, want: ErrBackfillEventNotFound},
		{name: "a service account", p: service, ev: open.ID, form: form, on: true, want: ErrBackfillForbidden},
		{name: "cancelled", p: editor, ev: cancelled.ID, form: form, on: true, want: ErrBackfillCancelled},
		{name: "ended", p: editor, ev: ended.ID, form: form, on: true, want: ErrBackfillEnded},
		{name: "not listed", p: editor, ev: open.ID, form: uuid.New(), on: true, want: ErrBackfillNotListed},
		{name: "in another Event", p: editor, ev: sharedA.ID, form: shared, on: true, want: ErrBackfillFormInUse},
		{name: "pulls off", p: editor, ev: open.ID, form: form, on: false, want: ErrBackfillOff},
		{name: "the form's owner in Forms", p: editor, ev: open.ID, form: form, on: true, forms: teamForm(RoleOwner)},
		{name: "an editor in Forms", p: editor, ev: open.ID, form: form, on: true, forms: teamForm(RoleEditor)},
		{name: "not on the form's team", p: editor, ev: open.ID, form: form, on: true, forms: stubForm{form: Form{FormID: form, AccessListed: true, Collaborators: []Collaborator{{UserID: uuid.New(), Role: RoleOwner}}}}, want: ErrFormsAccessDenied},
		{name: "a Forms without the team", p: editor, ev: open.ID, form: form, on: true, forms: stubForm{form: Form{FormID: form}}, want: ErrFormsAccessDenied},
		{name: "the form gone in Forms", p: editor, ev: open.ID, form: form, on: true, forms: stubForm{err: ErrNotFound}, want: ErrFormsAccessDenied},
		{name: "Forms down", p: editor, ev: open.ID, form: form, on: true, forms: stubForm{err: errors.New("formsfeed: Forms answered 503")}, want: ErrFormsUnavailable},
		{name: "no Forms client", p: editor, ev: open.ID, form: form, on: true, noForms: true, want: ErrFormsUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &memQueue{}
			forms := tc.forms
			if forms == nil && !tc.noForms {
				forms = viewer
			}
			b := &Backfills{
				Events: backfillEvents{events: []event.Event{open, draft, cancelled, ended, sharedA, sharedB}},
				Authz:  authz.NewAuthorizer(authz.DefaultPolicy()), Queue: q, On: tc.on, Forms: forms, Now: func() time.Time { return now },
			}
			if tc.again {
				b.Now = func() time.Time { return now.Add(-tc.after) }
				if _, err := b.Request(context.Background(), tc.p, tc.ev, tc.form); err != nil {
					t.Fatal(err)
				}
				b.Now = func() time.Time { return now }
			}
			queued, err := b.Request(context.Background(), tc.p, tc.ev, tc.form)
			if !errors.Is(err, tc.want) || (err == nil && queued == tc.again) {
				t.Fatalf("queued %t err %v; want %v", queued, err, tc.want)
			}
			var cooldown *BackfillCooldownError
			if errors.As(err, &cooldown) && cooldown.RetryAfter != BackfillCooldown-tc.after {
				t.Fatalf("retry after %v", cooldown.RetryAfter)
			}
			// A refusal before the queue queues nothing: no gathering of a
			// form the caller may not see in Forms.
			if err != nil && !tc.again && len(q.asked) != 0 {
				t.Fatalf("refused but queued %v", q.asked)
			}
		})
	}
}
