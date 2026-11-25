package skypass

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// An offline door's SkyPass QR (docs/offline-door.md): expired now, it is
// read at the moment the door took it; a scan taken after it expired is
// refused all the same.
func TestHolderFromReadsTheExpiryAtTheScan(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	signer := NewSigner(testECKey(t), time.Minute)
	minted := time.Date(2026, 12, 12, 19, 0, 0, 0, time.UTC)
	signer.Now = func() time.Time { return minted }
	svc := NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), signer)
	id := uuid.New()
	seedUser(t, store, id, "ada@example.com", "Ada", "Lovelace")
	tok, err := svc.Mint(context.Background(), authz.Principal{ID: id.String()})
	if err != nil {
		t.Fatal(err)
	}
	staff := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	signer.Now = func() time.Time { return minted.Add(2 * time.Hour) }
	if _, err := svc.HolderFrom(context.Background(), staff, tok.Value, ""); !errors.Is(err, ErrExpired) {
		t.Fatalf("now: %v", err)
	}
	got, err := svc.HolderFrom(WithScannedAt(context.Background(), minted.Add(30*time.Second)), staff, tok.Value, "")
	if err != nil || got.ID != id {
		t.Fatalf("at the scan: %+v %v", got, err)
	}
	if _, err := svc.HolderFrom(WithScannedAt(context.Background(), minted.Add(2*time.Minute)), staff, tok.Value, ""); !errors.Is(err, ErrExpired) {
		t.Fatalf("taken after it expired: %v", err)
	}
}
