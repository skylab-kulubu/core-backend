package user

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestBackfillYTU(t *testing.T) {
	t.Parallel()
	store := &countingStore{MemoryStore: NewMemoryStore()}
	svc := NewService(store)
	ctx := context.Background()
	ensure := func(p Profile) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, _, err := svc.Ensure(ctx, id, p); err != nil {
			t.Fatal(err)
		}
		return id
	}
	fresh := ensure(Profile{Email: "fresh@example.com"})
	withEmail := func(p Profile, email string) Profile { p.Email = email; return p }
	current := ensure(withEmail(ytuLogin("Mimarlık"), "current@std.yildiz.edu.tr"))
	stale := ensure(withEmail(ytuLogin("Matematik"), "stale@std.yildiz.edu.tr"))
	blocked := ensure(Profile{Email: "gone@example.com"})
	if _, err := store.RequestDeletion(ctx, blocked, nil); err != nil {
		t.Fatal(err)
	}
	unknownCode := ensure(Profile{Email: "code@example.com"})
	store.ytuWrites = 0

	accounts := []YTUAttributes{
		{ID: fresh, University: ytuUniversity, Department: "YapayZekaveVeriMühendisliği"},
		{ID: current, University: ytuUniversity, Department: "Mimarlık"},
		{ID: stale, University: ytuUniversity, Department: "022"},
		{ID: blocked, University: ytuUniversity, Department: "011"},
		{ID: unknownCode, University: ytuUniversity, Department: "0Z9"},
		{ID: uuid.New(), University: ytuUniversity, Department: "011"},
		{ID: uuid.New(), Department: "011"}, // no university: not YTÜ-linked, skipped
	}

	dry, err := BackfillYTU(ctx, store, accounts, false)
	if err != nil {
		t.Fatal(err)
	}
	want := YTUBackfillReport{
		Accounts: 6, Updated: 3, Unchanged: 1, NoRecord: 1, Blocked: 1,
		Unresolved: map[string]int{"0Z9": 1},
	}
	if !reflect.DeepEqual(dry, want) {
		t.Fatalf("dry run %+v, want %+v", dry, want)
	}
	if store.ytuWrites != 0 {
		t.Fatalf("a dry run wrote %d times", store.ytuWrites)
	}

	applied, err := BackfillYTU(ctx, store, accounts, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, want) || store.ytuWrites != 3 {
		t.Fatalf("apply %+v (writes %d)", applied, store.ytuWrites)
	}
	got, _ := store.Get(ctx, fresh)
	if !got.YTULinked || got.Department != "Yapay Zeka ve Veri Mühendisliği" || got.Faculty != "Bilgisayar ve Bilişim Bilimleri Fakültesi" {
		t.Fatalf("fresh %+v", got)
	}
	got, _ = store.Get(ctx, stale)
	if got.Department != "Fizik" || got.Faculty != "Fen-Edebiyat Fakültesi" {
		t.Fatalf("stale %+v", got)
	}

	// Running it again changes nothing.
	again, err := BackfillYTU(ctx, store, accounts, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Updated != 0 || again.Unchanged != 4 || store.ytuWrites != 3 {
		t.Fatalf("second run %+v (writes %d)", again, store.ytuWrites)
	}
}
