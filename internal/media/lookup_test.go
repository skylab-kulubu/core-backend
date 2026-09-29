package media_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// readCountingStore counts the store reads the lookup could make per
// Media.
type readCountingStore struct {
	*media.MemoryStore
	lookups, perMedia int
}

func (s *readCountingStore) LookUpKeys(ctx context.Context, keys []string, product authz.Product) ([]media.KeyMatch, error) {
	s.lookups++
	return s.MemoryStore.LookUpKeys(ctx, keys, product)
}

func (s *readCountingStore) HeldBy(ctx context.Context, id uuid.UUID, product authz.Product) (bool, error) {
	s.perMedia++
	return s.MemoryStore.HeldBy(ctx, id, product)
}

func (s *readCountingStore) Get(ctx context.Context, id uuid.UUID) (media.Media, error) {
	s.perMedia++
	return s.MemoryStore.Get(ctx, id)
}

func (s *readCountingStore) GetIncludingDeleted(ctx context.Context, id uuid.UUID) (media.Media, error) {
	s.perMedia++
	return s.MemoryStore.GetIncludingDeleted(ctx, id)
}

// A batch is one read, whatever it holds: legacy Media another person
// uploaded (the product's hold decides), the product's own, and addresses
// that name nothing.
func TestLookUpReadsABatchOnce(t *testing.T) {
	ctx := context.Background()
	store := &readCountingStore{MemoryStore: media.NewMemoryStore()}
	svc := media.NewService(store, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	addresses := []string{"junk"}
	for _, purpose := range []string{"legacy", "legacy", "cms_image", "event_cover"} {
		m, err := store.Create(ctx, media.Media{Name: "a.png", Type: "image/png", Kind: media.KindImage,
			Key: "images/" + uuid.NewString(), Purpose: purpose})
		if err != nil {
			t.Fatal(err)
		}
		addresses = append(addresses, "https://cdn.example.test/"+m.Key, "https://cdn.example.test/"+m.Key+"/card.jpg")
	}
	got, err := svc.LookUp(ctx, cmsService, func() (media.LookupRequest, error) {
		return media.LookupRequest{OnBehalfOf: uuid.New(), Addresses: addresses}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(addresses) || store.lookups != 1 || store.perMedia != 0 {
		t.Fatalf("%d results, %d lookups, %d reads per Media", len(got), store.lookups, store.perMedia)
	}
	// The person is not the uploader: the legacy ones are the product's
	// only once it holds them.
	for i, match := range got {
		if match.Address != addresses[i] {
			t.Fatalf("result %d is %q's", i, match.Address)
		}
		if match.MediaID != nil && (i < 5 || i > 6) {
			t.Fatalf("result %d names %s", i, match.MediaID)
		}
	}
}

// Only a product's service account that may attach gets its request read:
// anyone else is refused before, whatever the request holds.
func TestLookUpAuthorizesBeforeItReadsTheRequest(t *testing.T) {
	svc := media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "")
	unread := func() (media.LookupRequest, error) {
		t.Fatal("the request of a caller that may not look up was read")
		return media.LookupRequest{}, nil
	}
	for name, p := range map[string]authz.Principal{
		"person with the role": {ID: uuid.NewString(), Roles: []string{"media:attach"}},
		"service without it":   {ID: uuid.NewString(), Product: authz.ProductCMS},
		"admin":                {ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}},
	} {
		if _, err := svc.LookUp(context.Background(), p, unread); !errors.Is(err, media.ErrAttachForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	read := errors.New("unreadable")
	if _, err := svc.LookUp(context.Background(), cmsService, func() (media.LookupRequest, error) {
		return media.LookupRequest{}, read
	}); !errors.Is(err, read) {
		t.Fatalf("a product's unreadable request: %v", err)
	}
}
