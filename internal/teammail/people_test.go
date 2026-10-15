package teammail

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestDirectoryPeopleRecipient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	users := user.NewService(store)
	people := DirectoryPeople{Directory: dir, Accounts: store}

	active, keycloakOnly, noMail, disabled, pending, gone := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	dir.PutUser(identity.Person{ID: active, Email: "ada@example.test", FirstName: "Ada", LastName: "L"})
	if _, _, err := users.Ensure(ctx, active, user.Profile{Email: "ada@example.test", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	dir.PutUser(identity.Person{ID: keycloakOnly, Email: " grace@example.test ", FirstName: "Grace", LastName: "Hopper"})
	dir.PutUser(identity.Person{ID: noMail, FirstName: "No", LastName: "Mail"})
	dir.PutUser(identity.Person{ID: disabled, Email: "off@example.test"})
	if err := dir.DisableUser(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	dir.PutUser(identity.Person{ID: pending, Email: "pending@example.test"})
	if _, _, err := users.Ensure(ctx, pending, user.Profile{Email: "pending@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestDeletion(ctx, pending, nil); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		id   uuid.UUID
		want Recipient
		skip SkipReason
	}{
		// core's profile names the person, as every other read does.
		{id: active, want: Recipient{Email: "ada@example.test", FullName: "Ada Lovelace"}},
		{id: keycloakOnly, want: Recipient{Email: "grace@example.test", FullName: "Grace Hopper"}},
		{id: noMail, skip: SkipNoEmail},
		{id: disabled, skip: SkipInactive},
		{id: pending, skip: SkipInactive},
		{id: gone, skip: SkipInactive},
		{id: user.DeletedSubject, skip: SkipInactive},
	} {
		got, skip, err := people.Recipient(ctx, tc.id)
		if err != nil || got != tc.want || skip != tc.skip {
			t.Errorf("%s: %+v %q %v; want %+v %q", tc.id, got, skip, err, tc.want, tc.skip)
		}
	}

	if got := people.DisplayName(ctx, active); got != "Ada Lovelace" {
		t.Errorf("actor name %q", got)
	}
	if got := people.DisplayName(ctx, pending); got != "" {
		t.Errorf("pending actor name %q", got)
	}
	if got := people.DisplayName(ctx, gone); got != "" {
		t.Errorf("unknown actor name %q", got)
	}
}

type failingAccounts struct{ user.Store }

func (failingAccounts) Get(context.Context, uuid.UUID) (user.User, error) {
	return user.User{}, errors.New("database is down")
}

func TestDirectoryPeopleLookupFailureIsRetried(t *testing.T) {
	t.Parallel()
	people := DirectoryPeople{Directory: identity.NewMemory(), Accounts: failingAccounts{}}
	if _, _, err := people.Recipient(context.Background(), uuid.New()); err == nil {
		t.Fatal("a failed lookup skipped the mail")
	}
}
