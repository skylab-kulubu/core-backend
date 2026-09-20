package user

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEnsureCannotRepopulateDeletionPendingOrAnonymizedAccount(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	service := NewService(store)
	subjectID := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	if _, _, err := service.Ensure(ctx, subjectID, Profile{
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestDeletion(ctx, subjectID, nil); err != nil {
		t.Fatal(err)
	}

	if _, _, err := service.Ensure(ctx, subjectID, Profile{
		Email: "new@example.com", FirstName: "Repopulated", LastName: "Identity",
	}); !errors.Is(err, ErrAccountBlocked) {
		t.Fatalf("Ensure while deletion pending error = %v, want ErrAccountBlocked", err)
	}
	pending, err := store.Get(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.AccountState != AccountDeletionPending || pending.Email != "ada@example.com" {
		t.Fatalf("pending account was changed: %+v", pending)
	}

	if err := store.AnonymizeAccount(ctx, subjectID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Ensure(ctx, subjectID, Profile{
		Email: "old-token@example.com", FirstName: "Old", LastName: "Token",
	}); !errors.Is(err, ErrAccountBlocked) {
		t.Fatalf("Ensure after anonymization error = %v, want ErrAccountBlocked", err)
	}
	anonymized, err := store.Get(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if anonymized.AccountState != AccountAnonymized {
		t.Fatalf("account state = %q", anonymized.AccountState)
	}
	if anonymized.Email != "" || anonymized.FirstName != "" || anonymized.LastName != "" ||
		anonymized.Username != "" || anonymized.SchoolEmail != "" || anonymized.SkyNumber != "" ||
		anonymized.StudentCardUID != "" || anonymized.Linkedin != "" || anonymized.University != "" ||
		anonymized.Faculty != "" || anonymized.Department != "" || anonymized.Phone != "" ||
		anonymized.ProfilePictureID != nil || anonymized.ProfilePictureURL != "" {
		t.Fatalf("anonymized tombstone retained PII: %+v", anonymized)
	}
}

func TestRequestDeletionIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	subjectID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if _, _, err := NewService(store).Ensure(ctx, subjectID, Profile{Email: "grace@example.com"}); err != nil {
		t.Fatal(err)
	}

	first, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.SubjectID != subjectID {
		t.Fatalf("requests are not idempotent: first=%+v second=%+v", first, second)
	}
}
