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

	cases := []struct {
		name  string
		p     authz.Principal
		ev    uuid.UUID
		form  uuid.UUID
		on    bool
		want  error
		again bool
		after time.Duration
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &memQueue{}
			b := &Backfills{
				Events: backfillEvents{events: []event.Event{open, draft, cancelled, ended, sharedA, sharedB}},
				Authz:  authz.NewAuthorizer(authz.DefaultPolicy()), Queue: q, On: tc.on, Now: func() time.Time { return now },
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
		})
	}
}
