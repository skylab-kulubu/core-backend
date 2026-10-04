package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func formsReader() authz.Principal {
	return authz.Principal{ID: "forms", Roles: []string{"users:read"}}
}

// erasedPerson puts Ada in Keycloak and core, then runs core's side of the
// erasure up to the given state.
func erasedPerson(t *testing.T, dir *identity.Memory, store *user.MemoryStore, state user.AccountState) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	email := adaEmail(id)
	dir.PutUser(identity.Person{ID: id, Email: email, FirstName: "Ada", LastName: "Lovelace"})
	if _, _, err := store.Upsert(ctx, user.User{
		ID: id, Email: email, FirstName: "Ada", LastName: "Lovelace",
		SkyNumber: "SKY-" + id.String(), Phone: "+905551112233",
	}); err != nil {
		t.Fatal(err)
	}
	if state == user.AccountActive {
		return id
	}
	if _, err := store.RequestDeletion(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if state == user.AccountAnonymized {
		if err := store.AnonymizeAccount(ctx, id, time.Now().UTC(), nil); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func adaEmail(id uuid.UUID) string {
	return "ada-" + id.String() + "@example.com"
}

func assertErasedCard(t *testing.T, card identity.UserCard, id uuid.UUID, status user.ReadStatus) {
	t.Helper()
	want := identity.UserCard{Person: identity.Person{
		ID: id, FirstName: user.DeletedDisplayName,
		Status: status, DisplayName: user.DeletedDisplayName,
	}}
	if !reflect.DeepEqual(card, want) {
		t.Fatalf("card = %+v, want %+v", card, want)
	}
}

func TestService_GetUserAnswersAnErasedPersonAsDeleted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		keycloakGone bool
	}{
		{name: "identity still in Keycloak", keycloakGone: false},
		{name: "identity deleted from Keycloak", keycloakGone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, store, svc := setup(t)
			ctx := context.Background()
			id := erasedPerson(t, dir, store, user.AccountAnonymized)
			if tc.keycloakGone {
				if err := dir.DeleteUser(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range []authz.Principal{formsReader(), privileged()} {
				card, err := svc.GetUser(ctx, p, id)
				if err != nil {
					t.Fatalf("%s: %v", p.ID, err)
				}
				assertErasedCard(t, card, id, user.ReadStatusDeleted)
			}
		})
	}
}

func TestService_GetUserAnswersDeletionPendingWithoutPersonalData(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	id := erasedPerson(t, dir, store, user.AccountDeletionPending)
	for _, p := range []authz.Principal{formsReader(), privileged()} {
		card, err := svc.GetUser(ctx, p, id)
		if err != nil {
			t.Fatalf("%s: %v", p.ID, err)
		}
		assertErasedCard(t, card, id, user.ReadStatusDeletionPending)
	}
}

func TestService_GetUserResolvesThePlaceholderSubject(t *testing.T) {
	t.Parallel()
	_, _, svc := setup(t)
	card, err := svc.GetUser(context.Background(), formsReader(), user.DeletedSubject)
	if err != nil {
		t.Fatal(err)
	}
	assertErasedCard(t, card, user.DeletedSubject, user.ReadStatusDeleted)
}

// purgedStore is core's store after the internal hard purge: the row is gone
// and only the durable deletion marker is left.
type purgedStore struct {
	*user.MemoryStore
	purged uuid.UUID
}

func (s purgedStore) Get(ctx context.Context, id uuid.UUID) (user.User, error) {
	if id == s.purged {
		return user.User{}, user.ErrNotFound
	}
	return s.MemoryStore.Get(ctx, id)
}

func TestService_GetUserAnswersAHardPurgedPersonWithAMarkerAsDeleted(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	mem := user.NewMemoryStore()
	id := erasedPerson(t, dir, mem, user.AccountAnonymized)
	ctx := context.Background()
	if err := dir.DeleteUser(ctx, id); err != nil {
		t.Fatal(err)
	}
	svc := identity.NewService(dir, purgedStore{MemoryStore: mem, purged: id}, authz.NewAuthorizer(authz.DefaultPolicy()))

	card, err := svc.GetUser(ctx, formsReader(), id)
	if err != nil {
		t.Fatal(err)
	}
	assertErasedCard(t, card, id, user.ReadStatusDeleted)

	// A subject core never had a marker for is still not found.
	if _, err := svc.GetUser(ctx, formsReader(), uuid.New()); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unknown subject err = %v", err)
	}
}

func TestService_GetUserKeepsRefusingCallersWhoCannotReadUsers(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	id := erasedPerson(t, dir, store, user.AccountAnonymized)
	if err := dir.DeleteUser(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, target := range []uuid.UUID{id, user.DeletedSubject, uuid.New()} {
		if _, err := svc.GetUser(ctx, member(), target); !errors.Is(err, identity.ErrForbidden) {
			t.Fatalf("member read of %s err = %v", target, err)
		}
	}
}

func TestService_GetUserMarksAnActivePersonActive(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	id := erasedPerson(t, dir, store, user.AccountActive)
	for _, p := range []authz.Principal{formsReader(), privileged()} {
		card, err := svc.GetUser(ctx, p, id)
		if err != nil {
			t.Fatal(err)
		}
		if card.Status != user.ReadStatusActive || card.DisplayName != "Ada Lovelace" || card.FirstName != "Ada" || card.Email != adaEmail(id) {
			t.Fatalf("%s card = %+v", p.ID, card)
		}
	}
}

func TestUserCardJSONForADeletedPerson(t *testing.T) {
	t.Parallel()
	_, _, svc := setup(t)
	card, err := svc.GetUser(context.Background(), formsReader(), user.DeletedSubject)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id":          "00000000-0000-4000-8000-000000000000",
		"status":      "deleted",
		"displayName": "Silinmiş kullanıcı",
		"firstName":   "Silinmiş kullanıcı",
		"lastName":    "",
		"email":       "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON = %s", body)
	}
}

func TestService_ListUsersLeavesOutPeopleBeingOrAlreadyErased(t *testing.T) {
	t.Parallel()
	dir, store, svc := setup(t)
	ctx := context.Background()
	active := erasedPerson(t, dir, store, user.AccountActive)
	erasedPerson(t, dir, store, user.AccountDeletionPending)
	erasedPerson(t, dir, store, user.AccountAnonymized)
	for _, p := range []authz.Principal{formsReader(), privileged()} {
		for _, q := range []string{"", "ada"} {
			people, err := svc.ListUsers(ctx, p, q)
			if err != nil {
				t.Fatal(err)
			}
			if len(people) != 1 || people[0].ID != active || people[0].Status != user.ReadStatusActive || people[0].DisplayName != "Ada Lovelace" {
				t.Fatalf("%s q=%q people = %+v", p.ID, q, people)
			}
		}
	}
}

// failingStore is core's store when the database answers with an error.
type failingStore struct {
	*user.MemoryStore
	getErr, markerErr, accountsErr error
}

func (s failingStore) Accounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]user.Account, error) {
	if s.accountsErr != nil {
		return nil, s.accountsErr
	}
	return s.MemoryStore.Accounts(ctx, ids)
}

func (s failingStore) Get(ctx context.Context, id uuid.UUID) (user.User, error) {
	if s.getErr != nil {
		return user.User{}, s.getErr
	}
	return s.MemoryStore.Get(ctx, id)
}

func (s failingStore) AttributionState(ctx context.Context, id uuid.UUID) (user.AttributionState, error) {
	if s.markerErr != nil {
		return "", s.markerErr
	}
	return s.MemoryStore.AttributionState(ctx, id)
}

func TestService_GetUserFailsWhenCoreCannotSayWhetherThePersonIsErased(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	down := errors.New("database unavailable")
	for _, tc := range []struct {
		name  string
		store func(*user.MemoryStore) failingStore
	}{
		{name: "row read fails", store: func(m *user.MemoryStore) failingStore { return failingStore{MemoryStore: m, getErr: down} }},
		{name: "marker read fails", store: func(m *user.MemoryStore) failingStore { return failingStore{MemoryStore: m, markerErr: down} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := identity.NewMemory()
			// Keycloak still knows the person, core has no row: only the
			// marker says whether they were hard-purged.
			id := uuid.New()
			dir.PutUser(identity.Person{ID: id, Email: adaEmail(id), FirstName: "Ada", LastName: "Lovelace"})
			svc := identity.NewService(dir, tc.store(user.NewMemoryStore()), authz.NewAuthorizer(authz.DefaultPolicy()))
			for _, p := range []authz.Principal{formsReader(), privileged()} {
				if card, err := svc.GetUser(ctx, p, id); !errors.Is(err, down) {
					t.Fatalf("%s: card=%+v err=%v, want the store's error", p.ID, card, err)
				}
			}
		})
	}
}
