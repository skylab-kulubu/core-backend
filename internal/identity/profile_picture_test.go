package identity_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// pictured seeds three people in the directory and core: Ada with a
// profile picture, Bob without one, and Eda with a picture whose account
// erasure was requested. Ada holds the skyforms:access seat.
func pictured(t *testing.T, dir *identity.Memory, store *user.MemoryStore) (ada, bob, eda uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	ada = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	bob = uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	eda = uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	users := user.NewService(store)
	for _, p := range []identity.Person{
		{ID: ada, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"},
		{ID: bob, Email: "bob@example.com", FirstName: "Bob", LastName: "Builder"},
		{ID: eda, Email: "eda@example.com", FirstName: "Eda", LastName: "Erased"},
	} {
		dir.PutUser(p)
		if _, _, err := users.Ensure(ctx, p.ID, user.Profile{Email: p.Email, FirstName: p.FirstName, LastName: p.LastName}); err != nil {
			t.Fatal(err)
		}
	}
	for id, key := range map[uuid.UUID]string{ada: "https://cdn.example.test/ada", eda: "https://cdn.example.test/eda"} {
		if _, err := users.SetProfilePicture(ctx, id, uuid.New(), key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RequestDeletion(ctx, eda, nil); err != nil {
		t.Fatal(err)
	}
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	if err := dir.AddMember(ctx, "g-yk", ada); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetGroupClientRoles(ctx, "g-yk", []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}}); err != nil {
		t.Fatal(err)
	}
	return ada, bob, eda
}

// Every way of listing people answers each person's profile picture as the
// public team list does, to every caller who may read people (Account
// Center: the picture is visible in club apps). A person whose erasure was
// requested is not listed, picture and all.
func TestService_ListUsersCarriesProfilePictures(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ada, bob, _ := pictured(t, dir, store)
	reader := authz.Principal{ID: "reader", Roles: []string{"users:read"}}
	seat := identity.ClientRole{ClientID: "forms", Role: "skyforms:access"}

	for _, c := range []struct {
		name   string
		p      authz.Principal
		q      string
		seat   []identity.ClientRole
		listed []uuid.UUID
	}{
		{"privileged", privileged(), "", nil, []uuid.UUID{ada, bob}},
		{"privileged search", privileged(), "example.com", nil, []uuid.UUID{ada, bob}},
		{"privileged seat", privileged(), "", []identity.ClientRole{seat}, []uuid.UUID{ada}},
		{"users:read", reader, "", nil, []uuid.UUID{ada, bob}},
		{"users:read search", reader, "example.com", nil, []uuid.UUID{ada, bob}},
	} {
		people, err := svc.ListUsers(context.Background(), c.p, c.q, c.seat...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		byID := map[uuid.UUID]identity.Person{}
		for _, person := range people {
			byID[person.ID] = person
		}
		if len(byID) != len(c.listed) {
			t.Fatalf("%s: listed %+v", c.name, people)
		}
		for _, id := range c.listed {
			person, ok := byID[id]
			if !ok {
				t.Fatalf("%s: %s missing from %+v", c.name, id, people)
			}
			want := ""
			if id == ada {
				want = "https://cdn.example.test/ada"
			}
			if person.ProfilePictureURL != want {
				t.Fatalf("%s: %s answers picture %q, want %q", c.name, person.FirstName, person.ProfilePictureURL, want)
			}
		}
		raw, err := json.Marshal(people)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "/eda") {
			t.Fatalf("%s: an account being erased leaks its picture: %s", c.name, raw)
		}
		if strings.Count(string(raw), `"profilePictureUrl"`) != 1 {
			t.Fatalf("%s: a person without a picture answers one: %s", c.name, raw)
		}
	}
}

// The user card answers the picture too, to an admin and to a users:read
// caller; a person whose erasure was requested is answered as erased, with
// no picture.
func TestService_GetUserCarriesProfilePicture(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ada, bob, eda := pictured(t, dir, store)
	reader := authz.Principal{ID: "reader", Roles: []string{"users:read"}}
	ctx := context.Background()

	for _, p := range []authz.Principal{privileged(), reader} {
		card, err := svc.GetUser(ctx, p, ada)
		if err != nil {
			t.Fatal(err)
		}
		if card.ProfilePictureURL != "https://cdn.example.test/ada" {
			t.Fatalf("%v: card %+v", p.Roles, card)
		}
		none, err := svc.GetUser(ctx, p, bob)
		if err != nil {
			t.Fatal(err)
		}
		if none.ProfilePictureURL != "" || none.ProfilePictureSizes != nil {
			t.Fatalf("%v: a person without a picture answers %+v", p.Roles, none)
		}
		erased, err := svc.GetUser(ctx, p, eda)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(erased)
		if err != nil {
			t.Fatal(err)
		}
		if erased.Status != user.ReadStatusDeletionPending || strings.Contains(string(raw), "profilePicture") {
			t.Fatalf("%v: an account being erased answers %s", p.Roles, raw)
		}
	}
}
