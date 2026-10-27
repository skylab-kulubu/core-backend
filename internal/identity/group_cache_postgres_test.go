package identity_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// countingReader is Keycloak's "a person's Groups" call over the shared
// directory, counted per core process.
type countingReader struct {
	dir   *identity.Memory
	calls atomic.Int64
}

func (r *countingReader) GroupsForUser(ctx context.Context, id uuid.UUID) ([]identity.Group, error) {
	r.calls.Add(1)
	return r.dir.GroupsForUser(ctx, id)
}

// replica is one core process: its own Group overage cache, shared through
// Postgres, and its own identity service.
type replica struct {
	reader    *countingReader
	groups    *identity.OverageGroups
	svc       identity.Service
	listening chan struct{}
}

func startReplica(t *testing.T, ctx context.Context, dir *identity.Memory, notify identity.Notifier, connect func(context.Context) (*pgx.Conn, error)) replica {
	t.Helper()
	reader := &countingReader{dir: dir}
	groups := identity.NewOverageGroups(reader, identity.OverageOptions{})
	listening := make(chan struct{}, 8)
	shared := identity.NewSharedGroupCache(groups, notify, connect, identity.SharedGroupCacheOptions{
		Logf:      t.Logf,
		Retry:     20 * time.Millisecond,
		Listening: func() { listening <- struct{}{} },
	})
	runCtx, stop := context.WithCancel(ctx)
	done := shared.Run(runCtx)
	t.Cleanup(func() { stop(); <-done })
	select {
	case <-listening:
	case <-time.After(10 * time.Second):
		t.Fatal("the replica never listened")
	}
	svc := identity.NewServiceWithOptions(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{GroupCache: shared})
	return replica{reader: reader, groups: groups, svc: svc, listening: listening}
}

// A membership write through one core process reaches the other's Group
// overage cache: its next request reads the person's Groups again
// (horizontal-scale ticket 26).
func TestGroupWriteOnOneReplicaReachesTheOther(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	connect := identity.ListenConnect(pool)
	dir := identity.NewMemory()
	ada, bob := uuid.New(), uuid.New()
	dir.PutUser(identity.Person{ID: ada, Email: "ada@example.test"})
	dir.PutUser(identity.Person{ID: bob, Email: "bob@example.test"})
	dir.PutGroup(identity.Group{ID: "g-weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"})
	if err := dir.AddMember(ctx, "g-weblab-l", ada); err != nil {
		t.Fatal(err)
	}

	writer := startReplica(t, ctx, dir, pool, connect)
	other := startReplica(t, ctx, dir, pool, connect)
	for _, person := range []uuid.UUID{ada, bob} {
		if _, _, err := other.groups.Paths(ctx, person); err != nil {
			t.Fatal(err)
		}
	}
	if paths, cached, err := other.groups.Paths(ctx, ada); err != nil || !cached || len(paths) != 1 {
		t.Fatalf("before the write: %q cached=%v err=%v", paths, cached, err)
	}

	if err := writer.svc.RemoveMember(ctx, privileged(), "g-weblab-l", ada); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		paths, cached, err := other.groups.Paths(ctx, ada)
		if err != nil {
			t.Fatal(err)
		}
		if !cached {
			if len(paths) != 0 {
				t.Fatalf("after the write the other replica read %q", paths)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other replica still answers the Groups from before the write")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, cached, err := other.groups.Paths(ctx, bob); err != nil || !cached {
		t.Fatalf("someone else's Groups were forgotten too: cached=%v err=%v", cached, err)
	}

	// A rename changes every path: everyone is forgotten everywhere.
	if _, err := writer.svc.UpdateGroup(ctx, privileged(), "g-weblab-l", "KOORDINATORLER", nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, cached, err := other.groups.Paths(ctx, bob); err != nil {
			t.Fatal(err)
		} else if !cached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a rename on one replica left the other's answers")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// core's DATABASE_URL may carry the pool's own settings (pool_max_conns, ticket
// 19/34). The listener takes the pool's parsed settings, so it still connects;
// parsing the address again would send them to Postgres, which refuses them
// (review B1).
func TestListenerConnectsWhenTheAddressCarriesPoolSettings(t *testing.T) {
	base := testpostgres.Start(t)
	ctx := context.Background()
	address := base.Config().ConnString()
	if strings.Contains(address, "?") {
		address += "&pool_max_conns=4"
	} else {
		address += "?pool_max_conns=4"
	}
	pool, err := pgxpool.New(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if conn, err := pgx.Connect(ctx, address); err == nil {
		conn.Close(ctx)
		t.Fatal("Postgres took pool_max_conns as a runtime parameter; the test no longer shows the trap")
	}

	startReplica(t, ctx, identity.NewMemory(), pool, identity.ListenConnect(pool))
}

// While the listening connection is down nothing arrives, so the process
// forgets everyone when it listens again (review O3).
func TestListenerForgetsEveryoneWhenItListensAgain(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	dir := identity.NewMemory()
	ada := uuid.New()
	dir.PutUser(identity.Person{ID: ada, Email: "ada@example.test"})
	r := startReplica(t, ctx, dir, pool, identity.ListenConnect(pool))
	if _, _, err := r.groups.Paths(ctx, ada); err != nil {
		t.Fatal(err)
	}
	if _, cached, _ := r.groups.Paths(ctx, ada); !cached {
		t.Fatal("not cached before the break")
	}

	tag, err := pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() AND query = 'LISTEN '||$1::text`, identity.GroupsChangedChannel)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("terminate the listener: %v %v", tag, err)
	}
	select {
	case <-r.listening:
	case <-time.After(10 * time.Second):
		t.Fatal("the replica did not listen again")
	}
	if _, cached, err := r.groups.Paths(ctx, ada); err != nil || cached {
		t.Fatalf("after listening again: cached=%v err=%v", cached, err)
	}
}
