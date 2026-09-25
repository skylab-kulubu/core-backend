package user

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ytu"
)

const ytuUniversity = "Yıldız Teknik Üniversitesi"

// countingStore counts the YTÜ profile writes so the tests can see that core
// compares before it writes.
type countingStore struct {
	*MemoryStore
	ytuWrites int
}

func (s *countingStore) SetYTUProfile(ctx context.Context, id uuid.UUID, p ytu.Profile) (User, error) {
	s.ytuWrites++
	return s.MemoryStore.SetYTUProfile(ctx, id, p)
}

func ytuLogin(department string) Profile {
	return Profile{Email: "ada@std.yildiz.edu.tr", FirstName: "Ada", LastName: "Lovelace", University: ytuUniversity, Department: department}
}

func TestService_EnsureWritesTheYTUProfile(t *testing.T) {
	t.Parallel()
	store := &countingStore{MemoryStore: NewMemoryStore()}
	svc := NewService(store)
	id := uuid.New()
	ctx := context.Background()

	got, created, err := svc.Ensure(ctx, id, ytuLogin("MatematikMühendisliği"))
	if err != nil || !created {
		t.Fatalf("ensure: created=%v err=%v", created, err)
	}
	if !got.YTULinked || got.University != ytuUniversity || got.Department != "Matematik Mühendisliği" ||
		got.Faculty != "Bilgisayar ve Bilişim Bilimleri Fakültesi" {
		t.Fatalf("returned %+v", got)
	}
	stored, _ := store.Get(ctx, id)
	if stored.University != got.University || stored.Department != got.Department || stored.Faculty != got.Faculty || !stored.YTULinked {
		t.Fatalf("stored %+v", stored)
	}

	// The same claims again: nothing changed, so nothing is written.
	if _, _, err := svc.Ensure(ctx, id, ytuLogin("MatematikMühendisliği")); err != nil {
		t.Fatal(err)
	}
	if store.ytuWrites != 1 {
		t.Fatalf("YTÜ profile written %d times, want 1", store.ytuWrites)
	}

	// A transfer (yatay geçiş) arrives with the next login and overwrites.
	moved, _, err := svc.Ensure(ctx, id, ytuLogin("065"))
	if err != nil {
		t.Fatal(err)
	}
	if moved.Department != "Makine Mühendisliği" || moved.Faculty != "Makine Fakültesi" || store.ytuWrites != 2 {
		t.Fatalf("after transfer %+v (writes %d)", moved, store.ytuWrites)
	}
}

func TestService_EnsureOverwritesSelfTypedValuesOnFirstYTULogin(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.New()
	ctx := context.Background()
	if _, _, err := svc.Ensure(ctx, id, Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	uni, fac, dep := "Boğaziçi", "Mühendislik", "Bilgisayar"
	if _, err := svc.Patch(ctx, id, ProfilePatch{University: &uni, Faculty: &fac, Department: &dep}); err != nil {
		t.Fatalf("a person who is not YTÜ-linked edits freely: %v", err)
	}
	got, _, err := svc.Ensure(ctx, id, ytuLogin("Bilgisayar Mühendisliği"))
	if err != nil {
		t.Fatal(err)
	}
	if got.University != ytuUniversity || got.Department != "Bilgisayar Mühendisliği" || got.Faculty != "Bilgisayar ve Bilişim Bilimleri Fakültesi" {
		t.Fatalf("got %+v", got)
	}
}

func TestService_EnsureWithoutYTUClaimsKeepsTheYTUProfile(t *testing.T) {
	t.Parallel()
	store := &countingStore{MemoryStore: NewMemoryStore()}
	svc := NewService(store)
	id := uuid.New()
	ctx := context.Background()
	if _, _, err := svc.Ensure(ctx, id, ytuLogin("011")); err != nil {
		t.Fatal(err)
	}
	// Account Center's token carries no YTÜ claims; that says nothing about the person.
	got, _, err := svc.Ensure(ctx, id, Profile{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.YTULinked || got.Department != "Bilgisayar Mühendisliği" || store.ytuWrites != 1 {
		t.Fatalf("got %+v (writes %d)", got, store.ytuWrites)
	}
}

func TestService_EnsureClearsAnUnknownDepartmentCode(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.New()
	ctx := context.Background()
	if _, _, err := svc.Ensure(ctx, id, ytuLogin("Bilgisayar Mühendisliği")); err != nil {
		t.Fatal(err)
	}
	got, _, err := svc.Ensure(ctx, id, ytuLogin("0Z9"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.YTULinked || got.University != ytuUniversity || got.Department != "" || got.Faculty != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestService_YTUFieldsAreReadOnlyForYTULinkedPeople(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.New()
	ctx := context.Background()
	if _, _, err := svc.Ensure(ctx, id, ytuLogin("Matematik")); err != nil {
		t.Fatal(err)
	}
	other := "Başka"
	for name, patch := range map[string]ProfilePatch{
		"university": {University: &other},
		"faculty":    {Faculty: &other},
		"department": {Department: &other},
	} {
		if _, err := svc.Patch(ctx, id, patch); !errors.Is(err, ErrYTUManaged) {
			t.Fatalf("patch %s: err=%v, want ErrYTUManaged", name, err)
		}
	}
	if _, err := svc.Replace(ctx, id, ProfileUpdate{FirstName: "Ada", LastName: "Lovelace", University: ytuUniversity, Faculty: "Fen-Edebiyat Fakültesi", Department: "Fizik"}); !errors.Is(err, ErrYTUManaged) {
		t.Fatalf("replace with another department: err=%v", err)
	}

	// Sending the stored values back is not a change, so a full form still saves.
	link := "https://www.linkedin.com/in/ada"
	uni, fac, dep := " "+ytuUniversity, "Fen-Edebiyat Fakültesi", "Matematik"
	got, err := svc.Patch(ctx, id, ProfilePatch{Linkedin: &link, University: &uni, Faculty: &fac, Department: &dep})
	if err != nil {
		t.Fatalf("echoing the YTÜ values: %v", err)
	}
	if got.Linkedin != link || got.University != ytuUniversity || got.Department != "Matematik" {
		t.Fatalf("got %+v", got)
	}
	if _, err := svc.Replace(ctx, id, ProfileUpdate{FirstName: "Ada", LastName: "L", Linkedin: link, University: ytuUniversity, Faculty: "Fen-Edebiyat Fakültesi", Department: "Matematik"}); err != nil {
		t.Fatalf("replace echoing the YTÜ values: %v", err)
	}
}

func TestMemoryStore_ProfileWritesKeepTheYTUValues(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store)
	id := uuid.New()
	ctx := context.Background()
	if _, _, err := svc.Ensure(ctx, id, ytuLogin("Mimarlık")); err != nil {
		t.Fatal(err)
	}
	stale, _ := store.Get(ctx, id)
	stale.University, stale.Faculty, stale.Department = "", "", ""
	stale.Linkedin = "https://www.linkedin.com/in/ada"
	got, err := store.UpdateProfile(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if got.University != ytuUniversity || got.Department != "Mimarlık" || got.Faculty != "Mimarlık Fakültesi" || got.Linkedin == "" {
		t.Fatalf("a profile write from a stale read reverted the YTÜ values: %+v", got)
	}
}
