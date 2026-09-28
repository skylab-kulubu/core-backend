package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// The memory directory reports a person enabled until DisableUser, in every
// read that answers people. DisableUser only records it: it answers as it
// always has.
func TestMemoryReportsEnabledUntilDisableUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := identity.NewMemory()
	id := uuid.New()
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com", FirstName: "Ada"})
	group, err := dir.CreateGroup(ctx, "", "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.AddMember(ctx, group.ID, id); err != nil {
		t.Fatal(err)
	}
	role := identity.ClientRole{ClientID: "forms", Role: "skyforms:review"}
	if err := dir.AddUserExtraRole(ctx, id, role); err != nil {
		t.Fatal(err)
	}
	requireEnabled := func(when string, want bool) {
		t.Helper()
		got, err := dir.GetUser(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		reads := map[string][]identity.Person{"GetUser": {got}}
		if reads["ListUsers"], err = dir.ListUsers(ctx); err != nil {
			t.Fatal(err)
		}
		if reads["SearchUsers"], err = dir.SearchUsers(ctx, "ada", 10); err != nil {
			t.Fatal(err)
		}
		if reads["Members"], err = dir.Members(ctx, group.ID); err != nil {
			t.Fatal(err)
		}
		if reads["UsersWithClientRole"], err = dir.UsersWithClientRole(ctx, role.ClientID, role.Role); err != nil {
			t.Fatal(err)
		}
		for read, people := range reads {
			if len(people) != 1 || people[0].Enabled != want {
				t.Errorf("%s: %s answered %+v, want Enabled %v", when, read, people, want)
			}
		}
	}

	requireEnabled("put", true)
	if err := dir.DisableUser(ctx, id); err != nil {
		t.Fatalf("DisableUser = %v", err)
	}
	requireEnabled("disabled", false)
	if err := dir.DisableUser(ctx, uuid.New()); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("DisableUser of no one = %v, want ErrNotFound", err)
	}
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com", FirstName: "Ada"})
	requireEnabled("put again", true)
}
