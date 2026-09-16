package user

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestService_EnsureCreatesThenUpdates(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ctx := context.Background()

	first, created, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first ensure should create")
	}
	if first.Email != "a@example.com" || first.FirstName != "Ada" || first.ID != id {
		t.Fatalf("unexpected first upsert: %+v", first)
	}

	second, created, err := svc.Ensure(ctx, id, Profile{Email: "b@example.com", FirstName: "Ada", LastName: "Byron"})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second ensure should update")
	}
	if second.Email != "b@example.com" || second.LastName != "Byron" {
		t.Fatalf("unexpected second upsert: %+v", second)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "b@example.com" {
		t.Fatalf("store has %q", got.Email)
	}
}

func TestService_EnsureKeepsSchoolEmailWhenClaimEmpty(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	ctx := context.Background()

	if _, _, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com", SchoolEmail: "a@std.yildiz.edu.tr"}); err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.Ensure(ctx, id, Profile{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if second.SchoolEmail != "a@std.yildiz.edu.tr" {
		t.Fatalf("wiped school email: %+v", second)
	}

	hit, err := store.Search(ctx, "std.yildiz")
	if err != nil {
		t.Fatal(err)
	}
	if len(hit) != 1 || hit[0].ID != id {
		t.Fatalf("search %+v", hit)
	}
}

func TestService_EnsureConcurrentSameID(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Ensure(ctx, id, Profile{Email: "c@example.com", FirstName: "Grace", LastName: "Hopper"})
			if err != nil {
				t.Errorf("Ensure: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "c@example.com" {
		t.Fatalf("got %+v", got)
	}
}
