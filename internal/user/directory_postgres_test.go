package user_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// ListDirectory reads the active rows newest first in pages, with the match
// count, and narrows them by the query and the filters as the memory store
// does.
func TestPostgresListDirectory(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	memory := user.NewMemoryStore()

	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	type row struct {
		id      uuid.UUID
		u       user.User
		created time.Time
	}
	rows := []row{
		{u: user.User{Email: "ada@example.test", FirstName: "Ada", LastName: "Lovelace", Username: "ada", SkyNumber: "SKY-0001", SchoolEmail: "ada@std.yildiz.edu.tr"}, created: base.Add(-48 * time.Hour)},
		{u: user.User{Email: "grace@example.test", FirstName: "Grace", LastName: "Hopper", Username: "grace", SkyNumber: "SKY-0002"}, created: base.Add(time.Hour)},
		{u: user.User{Email: "lin@example.test", FirstName: "Lin", LastName: "Clark", Username: "lin", SkyNumber: "SKY-0003", SchoolEmail: "lin@std.yildiz.edu.tr"}, created: base.Add(2 * time.Hour)},
		// The same creation time as Lin: id decides.
		{u: user.User{Email: "joan@example.test", FirstName: "Joan", LastName: "Clarke", Username: "joan", SkyNumber: "SKY-0004", SchoolEmail: "joan@std.yildiz.edu.tr"}, created: base.Add(2 * time.Hour)},
	}
	for i := range rows {
		rows[i].id = uuid.New()
		rows[i].u.ID = rows[i].id
		for _, s := range []user.Store{store, memory} {
			if _, _, err := s.Upsert(ctx, rows[i].u); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET created_at = $2 WHERE id = $1`, rows[i].id, rows[i].created); err != nil {
			t.Fatal(err)
		}
	}
	// Someone being erased is never listed.
	gone := uuid.New()
	if _, _, err := store.Upsert(ctx, user.User{ID: gone, Email: "gone@example.test", FirstName: "Ada", LastName: "Gone"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestDeletion(ctx, gone, nil); err != nil {
		t.Fatal(err)
	}

	newest := []uuid.UUID{rows[2].id, rows[3].id}
	slices.SortFunc(newest, func(a, b uuid.UUID) int { return compareUUID(a, b) })
	wantOrder := append(newest, rows[1].id, rows[0].id)

	ids := func(page user.DirectoryPage) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(page.Users))
		for _, u := range page.Users {
			out = append(out, u.ID)
		}
		return out
	}
	read := func(q user.DirectoryQuery) user.DirectoryPage {
		t.Helper()
		page, err := store.ListDirectory(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}

	all := read(user.DirectoryQuery{Limit: 10})
	if all.Total != 4 || !slices.Equal(ids(all), wantOrder) {
		t.Fatalf("all = %d %v, want 4 %v", all.Total, ids(all), wantOrder)
	}
	second := read(user.DirectoryQuery{Offset: 2, Limit: 2})
	if second.Total != 4 || !slices.Equal(ids(second), wantOrder[2:]) {
		t.Fatalf("second page = %d %v", second.Total, ids(second))
	}
	if past := read(user.DirectoryQuery{Offset: 8, Limit: 2}); past.Total != 4 || len(past.Users) != 0 {
		t.Fatalf("past the end = %+v", past)
	}
	if got := read(user.DirectoryQuery{Query: "  CLARK ", Limit: 10}); got.Total != 2 {
		t.Fatalf("search on the last name = %v", ids(got))
	}
	if got := read(user.DirectoryQuery{Query: "sky-0002", Limit: 10}); !slices.Equal(ids(got), []uuid.UUID{rows[1].id}) {
		t.Fatalf("search on the Sky number = %v", ids(got))
	}
	if got := read(user.DirectoryQuery{Query: "sky-0002", NamesOnly: true, Limit: 10}); got.Total != 0 {
		t.Fatalf("names-only search on the Sky number = %v", ids(got))
	}
	if got := read(user.DirectoryQuery{Query: "grace hop", NamesOnly: true, Limit: 10}); !slices.Equal(ids(got), []uuid.UUID{rows[1].id}) {
		t.Fatalf("names-only search on the full name = %v", ids(got))
	}
	if got := read(user.DirectoryQuery{NoSchoolEmail: true, Limit: 10}); !slices.Equal(ids(got), []uuid.UUID{rows[1].id}) {
		t.Fatalf("no school e-mail = %v", ids(got))
	}
	if got := read(user.DirectoryQuery{CreatedSince: base, Limit: 10}); got.Total != 3 || !slices.Equal(ids(got), wantOrder[:3]) {
		t.Fatalf("created since = %d %v", got.Total, ids(got))
	}
	if got := read(user.DirectoryQuery{Query: "ada", Limit: 10}); !slices.Equal(ids(got), []uuid.UUID{rows[0].id}) {
		t.Fatalf("the person being erased is listed: %v", ids(got))
	}
	if got := all.Users[3]; got.SkyNumber != "SKY-0001" || got.SchoolEmail != "ada@std.yildiz.edu.tr" || !got.CreatedAt.Equal(rows[0].created) {
		t.Fatalf("row = %+v", got)
	}

	// The memory store answers the same, but for the creation times it
	// stamps itself: compare what does not depend on them.
	for _, q := range []user.DirectoryQuery{
		{Query: "clark", Limit: 10},
		{Query: "sky-0002", NamesOnly: true, Limit: 10},
		{Query: "grace hop", NamesOnly: true, Limit: 10},
		{NoSchoolEmail: true, Limit: 10},
	} {
		pg, err := store.ListDirectory(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		mem, err := memory.ListDirectory(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		a, b := ids(pg), ids(mem)
		slices.SortFunc(a, compareUUID)
		slices.SortFunc(b, compareUUID)
		if pg.Total != mem.Total || !slices.Equal(a, b) {
			t.Fatalf("%+v: postgres %d %v, memory %d %v", q, pg.Total, a, mem.Total, b)
		}
	}
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
