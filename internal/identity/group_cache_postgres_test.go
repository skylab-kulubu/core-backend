package identity_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	reader *countingReader
	groups *identity.OverageGroups
	svc    identity.Service
}

func startReplica(t *testing.T, ctx context.Context, dir *identity.Memory, notify identity.Notifier, connect func(context.Context) (*pgx.Conn, error)) replica {
	t.Helper()
	reader := &countingReader{dir: dir}
	groups := identity.NewOverageGroups(reader, identity.OverageOptions{})
	listening := make(chan struct{}, 1)
	shared := identity.NewSharedGroupCache(groups, notify, connect, identity.SharedGroupCacheOptions{
		Logf:      t.Logf,
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
	return replica{reader: reader, groups: groups, svc: svc}
}

// A membership write through one core process reaches the other's Group
// overage cache: its next request reads the person's Groups again
// (horizontal-scale ticket 26).
func TestGroupWriteOnOneReplicaReachesTheOther(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	connect := func(ctx context.Context) (*pgx.Conn, error) { return pgx.ConnectConfig(ctx, pool.Config().ConnConfig) }
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
