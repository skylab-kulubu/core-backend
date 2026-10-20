package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Deleting (archiving) an Event: a draft by its editors; a published or
// cancelled Event only by who holds event:manage (YK, ADMIN; ADR 0066,
// decision 3, ticket 05). Restore does not change.
func TestDeleteOnlyADraftUnlessEventManage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	dk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/DK"}}
	for _, tc := range []struct {
		status event.Status
		who    string
		p      authz.Principal
		want   error
	}{
		{event.StatusDraft, "leader", leader, nil},
		{event.StatusPublished, "leader", leader, event.ErrNotDraft},
		{event.StatusCancelled, "leader", leader, event.ErrNotDraft},
		{event.StatusDraft, "yk", yk, nil},
		{event.StatusPublished, "yk", yk, nil},
		{event.StatusCancelled, "yk", yk, nil},
		{event.StatusDraft, "dk", dk, event.ErrForbidden},
		{event.StatusPublished, "dk", dk, event.ErrForbidden},
	} {
		store, svc := setup(t)
		created, err := store.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: tc.status})
		if err != nil {
			t.Fatal(err)
		}
		err = svc.Delete(ctx, tc.p, created.ID)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s %s: %v, want %v", tc.who, tc.status, err, tc.want)
		}
		stored, getErr := store.GetIncludingArchived(ctx, created.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if archived := stored.ArchivedAt != nil; archived != (tc.want == nil) {
			t.Fatalf("%s %s: archived %v", tc.who, tc.status, archived)
		}
		if tc.want != nil {
			continue
		}
		// Idempotent, and restored as before.
		if err := svc.Delete(ctx, tc.p, created.ID); err != nil {
			t.Fatalf("%s %s again: %v", tc.who, tc.status, err)
		}
		if _, err := svc.Restore(ctx, tc.p, created.ID); err != nil {
			t.Fatalf("%s %s restore: %v", tc.who, tc.status, err)
		}
	}
}

// A published Event archived by YK stays deletable again by its Leader:
// a second delete of an archived Event changes nothing.
func TestDeleteOfAnArchivedEventIsIdempotentForItsLeader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, svc := setup(t)
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	created, err := store.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ctx, created.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, leader, created.ID); err != nil {
		t.Fatalf("leader on archived published: %v", err)
	}
	if err := svc.Delete(ctx, leader, uuid.New()); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
