package mailpref_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/mailpref"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func on(v bool) *bool { return &v }

// The store contract, on the table and in memory alike: a value replaces
// the category's row (the master switch and club rows too, whose key has
// NULLs), nil deletes it, erasure deletes every row of the person and a
// second erasure finds none, a deleted team's rows go.
func TestStores(t *testing.T) {
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	stores := map[string]mailpref.Store{"postgres": mailpref.NewPostgresStore(pool), "memory": mailpref.NewMemoryStore()}
	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			person, other := uuid.New(), uuid.New()
			now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
			all := []mailpref.Category{
				mailpref.Master(), mailpref.Club(mailpref.KindAnnouncement), mailpref.Club(mailpref.KindNews),
				mailpref.Team("g-weblab", mailpref.KindAnnouncement), mailpref.Team("g-weblab", mailpref.KindNews),
			}
			for _, enabled := range []bool{true, false} {
				var changes []mailpref.Change
				for _, c := range all {
					changes = append(changes, mailpref.Change{Category: c, Enabled: on(enabled)})
				}
				if err := store.Apply(ctx, person, changes, mailpref.SourceAccountCenter, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Apply(ctx, person, []mailpref.Change{{Category: mailpref.Master(), Enabled: on(false)}}, mailpref.SourceUnsubscribeLink, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			rows, err := store.List(ctx, person)
			if err != nil || len(rows) != len(all) {
				t.Fatalf("rows = %+v %v", rows, err)
			}
			for _, row := range rows {
				if row.Enabled || !slices.Contains(all, row.Category) {
					t.Fatalf("row %+v", row)
				}
				if row.Category == mailpref.Master() && (row.Source != mailpref.SourceUnsubscribeLink || !row.UpdatedAt.Equal(now.Add(time.Hour))) {
					t.Fatalf("master row %+v", row)
				}
			}
			if err := store.Apply(ctx, person, []mailpref.Change{{Category: mailpref.Master()}, {Category: mailpref.Club(mailpref.KindNews)}, {Category: mailpref.Team("g-weblab", mailpref.KindNews)}}, mailpref.SourceAccountCenter, now); err != nil {
				t.Fatal(err)
			}
			if rows, _ := store.List(ctx, person); len(rows) != 2 {
				t.Fatalf("after forgetting: %+v", rows)
			}
			if err := store.Apply(ctx, other, []mailpref.Change{{Category: mailpref.Team("g-airlab", mailpref.KindNews), Enabled: on(true)}, {Category: mailpref.Team("g-weblab", mailpref.KindNews), Enabled: on(true)}}, mailpref.SourceAdmin, now); err != nil {
				t.Fatal(err)
			}
			if ids, err := store.TeamGroupIDs(ctx); err != nil || !slices.Equal(ids, []string{"g-airlab", "g-weblab"}) {
				t.Fatalf("ids = %v %v", ids, err)
			}
			if n, err := store.DeleteTeams(ctx, []string{"g-weblab"}); err != nil || n != 2 {
				t.Fatalf("delete teams: %d %v", n, err)
			}
			if n, err := store.EraseSubject(ctx, person); err != nil || n != 1 {
				t.Fatalf("erase: %d %v", n, err)
			}
			if n, err := store.EraseSubject(ctx, person); err != nil || n != 0 {
				t.Fatalf("erase again: %d %v", n, err)
			}
			if rows, _ := store.List(ctx, other); len(rows) != 1 || rows[0].Source != mailpref.SourceAdmin {
				t.Fatalf("other = %+v", rows)
			}
		})
	}

	// The table refuses a row for a person being erased.
	ctx := context.Background()
	users := user.NewPostgresStore(pool)
	holder := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, holder, user.Profile{Email: "pref-store@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.RequestDeletion(ctx, holder, nil); err != nil {
		t.Fatal(err)
	}
	err := mailpref.NewPostgresStore(pool).Apply(ctx, holder, []mailpref.Change{{Category: mailpref.Master(), Enabled: on(false)}}, mailpref.SourceUnsubscribeLink, time.Now())
	if !errors.Is(err, mailpref.ErrSubjectInactive) {
		t.Fatalf("err = %v", err)
	}
}
