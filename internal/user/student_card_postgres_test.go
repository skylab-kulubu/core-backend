package user_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A profile write that read the row before the card was removed must not
// bring the card back (KVKK s6 §5.11: a removed card's UID is deleted).
func TestPostgresProfileWriteDoesNotRestoreARemovedCard(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	id := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, id, user.Profile{
		Email: "card@example.com", FirstName: "Ada", LastName: "Lovelace", Username: "ada-card",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetStudentCardUID(ctx, id, "04AABBCCDD"); err != nil {
		t.Fatal(err)
	}
	stale, err := store.Get(ctx, id)
	if err != nil || stale.StudentCardUID != "04AABBCCDD" {
		t.Fatalf("bound card: %v", err)
	}

	if _, err := store.SetStudentCardUID(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	stale.Linkedin = "https://www.linkedin.com/in/ada"
	got, err := store.UpdateProfile(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if got.StudentCardUID != "" || got.StudentCardLinked || got.Linkedin != stale.Linkedin {
		t.Fatalf("after profile write: linked=%v linkedin=%q", got.StudentCardLinked, got.Linkedin)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT student_card_uid IS NULL FROM users WHERE id = $1`, id).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("student_card_uid is not NULL after the profile write")
	}
	if _, err := store.FindByStudentCardUID(ctx, "04AABBCCDD"); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("removed card still finds its holder: %v", err)
	}
}
