package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// An unsearched full list reads core's rows and writes only what the
// directory changes in them: a person core has no row for gets one, with a
// Sky number, a changed Keycloak e-mail is stored, and every other row is
// left as it was, however often the list is read.
func TestService_ListUsersWritesOnlyWhatTheDirectoryChanges(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	stored := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	missing := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	moved := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	for _, p := range []identity.Person{
		{ID: stored, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", Username: "ada"},
		{ID: moved, Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper", Username: "grace"},
	} {
		dir.PutUser(p)
		if _, _, err := user.NewService(store).Ensure(ctx, p.ID, user.Profile{
			Email: p.Email, FirstName: p.FirstName, LastName: p.LastName, Username: p.Username,
		}); err != nil {
			t.Fatal(err)
		}
	}
	dir.PutUser(identity.Person{ID: missing, Email: "lin@example.com", FirstName: "Lin", LastName: "Yu", Username: "lin"})
	dir.PutUser(identity.Person{ID: moved, Email: "grace@new.example.com", FirstName: "Grace", LastName: "Hopper", Username: "grace"})
	row := func(id uuid.UUID) user.User {
		t.Helper()
		u, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	before := row(stored)
	// The memory store stamps a write with the clock; a pause makes any
	// write show.
	time.Sleep(2 * time.Millisecond)

	people, err := svc.ListUsers(ctx, privileged(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 3 {
		t.Fatalf("people = %+v", people)
	}
	if got := row(stored); !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("the list rewrote an unchanged row: updated_at %v -> %v", before.UpdatedAt, got.UpdatedAt)
	}
	if got := row(missing); got.SkyNumber == "" || got.Email != "lin@example.com" {
		t.Fatalf("missing person's row = %+v", got)
	}
	if got := row(moved); got.Email != "grace@new.example.com" {
		t.Fatalf("changed e-mail not stored: %+v", got)
	}
	for _, p := range people {
		if p.ID == missing && p.SkyNumber != row(missing).SkyNumber {
			t.Fatalf("the list did not answer the new Sky number: %+v", p)
		}
	}

	first := map[uuid.UUID]time.Time{}
	for _, id := range []uuid.UUID{stored, missing, moved} {
		first[id] = row(id).UpdatedAt
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := svc.ListUsers(ctx, privileged(), ""); err != nil {
		t.Fatal(err)
	}
	for id, at := range first {
		if got := row(id).UpdatedAt; !got.Equal(at) {
			t.Fatalf("the second list wrote %s: updated_at %v -> %v", id, at, got)
		}
	}
}
