package apppref_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/apppref"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func write(value string) apppref.Write {
	return apppref.Write{Value: json.RawMessage(value), SchemaVersion: 1}
}

// The store contract, on the table and in memory alike: a write makes the
// row at version 1 and each later write adds one; If-Match holds the
// stored version; the app's quota counts the other keys; the value comes
// back compact; a person's apps are summarised without values; deleting a
// key, an app and the person each leaves the others alone.
func TestStores(t *testing.T) {
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	stores := map[string]apppref.Store{"postgres": apppref.NewPostgresStore(pool), "memory": apppref.NewMemoryStore()}
	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			person, other := uuid.New(), uuid.New()
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

			first, err := store.Put(ctx, person, "forms", "forms.list", write(`{"details":false,"sort":["updatedAt","desc"]}`), "skyforms", now)
			if err != nil || first.Version != 1 || first.SchemaVersion != 1 || !first.UpdatedAt.Equal(now) {
				t.Fatalf("first %+v %v", first, err)
			}
			second, err := store.Put(ctx, person, "forms", "forms.list", apppref.Write{Value: json.RawMessage(`{"details":true}`), SchemaVersion: 2, Match: &apppref.Match{Version: 1}}, "skyforms", now.Add(time.Minute))
			if err != nil || second.Version != 2 || second.SchemaVersion != 2 || string(second.Value) != `{"details":true}` {
				t.Fatalf("second %+v %v", second, err)
			}
			if _, err := store.Put(ctx, person, "forms", "forms.list", apppref.Write{Value: json.RawMessage(`1`), SchemaVersion: 1, Match: &apppref.Match{Version: 1}}, "skyforms", now); !errors.Is(err, apppref.ErrConflict) {
				t.Fatalf("stale match: %v", err)
			}
			if _, err := store.Put(ctx, person, "forms", "missing", apppref.Write{Value: json.RawMessage(`1`), SchemaVersion: 1, Match: &apppref.Match{Any: true}}, "skyforms", now); !errors.Is(err, apppref.ErrConflict) {
				t.Fatalf("match on a missing key: %v", err)
			}
			if _, err := store.Put(ctx, person, "forms", "share-format", write(`"utm"`), "skyforms", now.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put(ctx, person, "panel", "density", write(`"compact"`), "admin", now); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put(ctx, other, "forms", "forms.list", write(`{"details":false}`), "skyforms", now); err != nil {
				t.Fatal(err)
			}

			items, err := store.List(ctx, person, "forms", "")
			if err != nil || len(items) != 2 || items[0].Key != "forms.list" || items[1].Key != "share-format" || string(items[1].Value) != `"utm"` {
				t.Fatalf("list %+v %v", items, err)
			}
			one, err := store.List(ctx, person, "forms", "share-format")
			if err != nil || len(one) != 1 {
				t.Fatalf("one key %+v %v", one, err)
			}
			sums, err := store.Summaries(ctx, person)
			if err != nil || len(sums) != 2 || sums[0].App != "forms" || sums[0].Keys != 2 || !sums[0].UpdatedAt.Equal(now.Add(2*time.Minute)) || sums[1].App != "panel" {
				t.Fatalf("summaries %+v %v", sums, err)
			}

			if err := store.DeleteKey(ctx, person, "forms", "share-format"); err != nil {
				t.Fatal(err)
			}
			if err := store.DeleteKey(ctx, person, "forms", "share-format"); err != nil {
				t.Fatalf("deleting a missing key: %v", err)
			}
			if n, err := store.DeleteApp(ctx, person, "forms"); err != nil || n != 1 {
				t.Fatalf("delete app %d %v", n, err)
			}
			if n, err := store.EraseSubject(ctx, person); err != nil || n != 1 {
				t.Fatalf("erase %d %v", n, err)
			}
			if n, err := store.EraseSubject(ctx, person); err != nil || n != 0 {
				t.Fatalf("erase again %d %v", n, err)
			}
			if items, err := store.List(ctx, other, "forms", ""); err != nil || len(items) != 1 {
				t.Fatalf("another person's rows %+v %v", items, err)
			}
		})
	}
}

// The quota counts the app's other keys, in one transaction per person and
// app: writes of new keys that race never go past the key limit.
func TestPostgresStoreQuotaHoldsUnderConcurrentWrites(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := apppref.NewPostgresStore(pool)
	person := uuid.New()
	var wg sync.WaitGroup
	errs := make(chan error, apppref.MaxKeysPerApp+8)
	for i := range apppref.MaxKeysPerApp + 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Put(ctx, person, "forms", "k"+strings.Repeat("x", i), write(`1`), "skyforms", time.Now())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	quota := 0
	for err := range errs {
		switch {
		case errors.Is(err, apppref.ErrQuota):
			quota++
		case err != nil:
			t.Fatal(err)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_preferences WHERE user_id = $1`, person).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != apppref.MaxKeysPerApp || quota != 8 {
		t.Fatalf("rows=%d refused=%d", rows, quota)
	}
}

// A person being erased gets no new row: the table's trigger answers and the
// store says ErrSubjectInactive. A person without a core row may write.
func TestPostgresStoreRefusesAnInactiveSubject(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := apppref.NewPostgresStore(pool)
	if _, err := store.Put(ctx, uuid.New(), "forms", "forms.list", write(`1`), "skyforms", time.Now()); err != nil {
		t.Fatalf("person without a core row: %v", err)
	}
	users := user.NewPostgresStore(pool)
	holder := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, holder, user.Profile{Email: "app-pref@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, holder, "forms", "forms.list", write(`1`), "skyforms", time.Now()); err != nil {
		t.Fatalf("active person: %v", err)
	}
	if _, err := users.RequestDeletion(ctx, holder, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, holder, "forms", "other", write(`1`), "skyforms", time.Now()); !errors.Is(err, apppref.ErrSubjectInactive) {
		t.Fatalf("person being erased: %v", err)
	}
	if n, err := store.EraseSubject(ctx, holder); err != nil || n != 1 {
		t.Fatalf("erase %d %v", n, err)
	}
}
