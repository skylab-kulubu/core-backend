package user_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// FindBySchoolEmail finds active accounts by their school e-mail, whatever
// its case, as the memory store does; an empty address finds nobody.
func TestFindBySchoolEmail(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for name, store := range map[string]interface {
		Upsert(context.Context, user.User) (user.User, bool, error)
		FindBySchoolEmail(context.Context, string) ([]user.User, error)
	}{"postgres": user.NewPostgresStore(pool), "memory": user.NewMemoryStore()} {
		grace := user.User{ID: uuid.New(), Email: "grace@example.test", FirstName: "Grace", LastName: "Hopper", SchoolEmail: "grace@std.yildiz.edu.tr"}
		if _, _, err := store.Upsert(ctx, grace); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Upsert(ctx, user.User{ID: uuid.New(), Email: "lin@example.test", FirstName: "Lin", LastName: "Clark"}); err != nil {
			t.Fatal(err)
		}
		found, err := store.FindBySchoolEmail(ctx, " GRACE@std.yildiz.edu.tr ")
		if err != nil || len(found) != 1 || found[0].ID != grace.ID {
			t.Fatalf("%s: %+v %v", name, found, err)
		}
		for _, none := range []string{"", "grace@example.test", "lin@std.yildiz.edu.tr"} {
			if found, err := store.FindBySchoolEmail(ctx, none); err != nil || len(found) != 0 {
				t.Fatalf("%s %q: %+v %v", name, none, found, err)
			}
		}
	}
}
