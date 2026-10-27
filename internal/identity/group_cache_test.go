package identity_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

type recordedNotify struct {
	mu       sync.Mutex
	payloads []string
	err      error
}

func (r *recordedNotify) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(args) == 2 && args[0] == identity.GroupsChangedChannel {
		r.payloads = append(r.payloads, args[1].(string))
	}
	return pgconn.CommandTag{}, r.err
}

func noConnect(context.Context) (*pgx.Conn, error) { return nil, errors.New("no database") }

// Forget and ForgetAll drop the process's own answers at once and tell the
// other processes; a database that cannot be told changes nothing locally.
func TestSharedGroupCacheForgetsHereAndTellsTheOthers(t *testing.T) {
	t.Parallel()
	spy := &groupCacheSpy{}
	notify := &recordedNotify{}
	shared := identity.NewSharedGroupCache(spy, notify, noConnect, identity.SharedGroupCacheOptions{Origin: "a"})
	person := uuid.New()

	shared.Forget(person)
	shared.ForgetAll()
	notify.err = errors.New("database down")
	shared.Forget(person)

	if !slices.Equal(spy.forgotten, []uuid.UUID{person, person}) || spy.all != 1 {
		t.Fatalf("locally forgot %v and everyone %d times", spy.forgotten, spy.all)
	}
	want := []string{"a " + person.String(), "a *", "a " + person.String()}
	if !slices.Equal(notify.payloads, want) {
		t.Fatalf("told %q, want %q", notify.payloads, want)
	}
}

// What another process says it changed is dropped here; what this process
// said itself, and anything else, is not acted on twice.
func TestSharedGroupCacheAppliesWhatAnotherProcessSaid(t *testing.T) {
	t.Parallel()
	spy := &groupCacheSpy{}
	shared := identity.NewSharedGroupCache(spy, &recordedNotify{}, noConnect, identity.SharedGroupCacheOptions{Origin: "a"})
	person := uuid.New()

	for _, payload := range []string{"b " + person.String(), "a " + person.String(), "b *", "a *", "b not-a-uuid", "", "b"} {
		shared.ApplyForTest(payload)
	}
	if !slices.Equal(spy.forgotten, []uuid.UUID{person}) || spy.all != 1 {
		t.Fatalf("forgot %v and everyone %d times, want [%s] and 1", spy.forgotten, spy.all, person)
	}
}
