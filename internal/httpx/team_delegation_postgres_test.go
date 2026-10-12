package httpx_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// slowAdds is a directory whose member adds take a while, as Keycloak's
// do under load: delegated writes then queue behind the Team code lock.
type slowAdds struct {
	*identity.Memory
	delay time.Duration
}

func (s slowAdds) AddMember(ctx context.Context, groupID string, userID uuid.UUID) error {
	time.Sleep(s.delay)
	return s.Memory.AddMember(ctx, groupID, userID)
}

// Delegated writes cannot use up core's database pool (review of #235, B2):
// with a pool of 4, eight callers who lead nothing and six Leaders writing
// at once all finish in bounded time, an unrelated query does not wait for
// them, every refusal keeps its denied row and every Leader's add is made
// and recorded. Callers who lead nothing are refused before the lock; the
// Leaders wait for it one at a time, holding no connection while they wait.
func TestTeamDelegationWritesKeepThePoolFree(t *testing.T) {
	big := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), big); err != nil {
		t.Fatal(err)
	}
	config := big.Config().Copy()
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	e := newDelegationEnv(t, delegationSetup{on: true})
	leaders := make([]string, 6)
	for i := range leaders {
		leaders[i] = "pgLeader" + string(rune('a'+i))
		e.person(leaders[i], "/UYELER/ARGE/WEBLAB/LIDERLER")
	}
	outsiders := make([]string, 8)
	for i := range outsiders {
		outsiders[i] = "pgOutsider" + string(rune('a'+i))
		e.person(outsiders[i], "")
	}
	targets := make([]string, len(leaders))
	for i := range targets {
		targets[i] = "pgNew" + string(rune('a'+i))
		e.person(targets[i], "")
	}
	audit := teamaudit.NewPostgres(pool)
	policy := authz.DefaultPolicy()
	policy.TeamDelegation = true
	az := authz.NewAuthorizer(policy)
	delegation := identity.NewTeamDelegation(policy, e.dir, e.dir, identity.TeamDelegationOptions{})
	options := identity.Options{TeamCodes: teamcode.NewPostgresLedger(pool), TeamAudit: audit, Delegation: delegation}
	deps := memoryDepsDecidedBy(e.dir, options, az)
	deps.Identity = identity.NewServiceWithOptions(identity.AuditedDirectory(slowAdds{e.dir, 250 * time.Millisecond}, audit, nil), e.users, az, options)
	deps.ParseToken = e.keys.Parse()
	e.app = httpx.New(deps)

	tokens := map[string]string{}
	for _, name := range append(append([]string{}, leaders...), outsiders...) {
		tokens[name] = e.token(name)
	}
	start := time.Now()
	var wg sync.WaitGroup
	statuses := make(chan [2]any, len(leaders)+len(outsiders))
	for i, name := range leaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := e.send(tokens[name], fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB")+"/members", `{"userId":"`+e.people[targets[i]].String()+`"}`)
			statuses <- [2]any{name, got.status}
		}()
	}
	for _, name := range outsiders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := e.send(tokens[name], fiber.MethodPost, e.group("/UYELER/ARGE/WEBLAB")+"/members", `{"userId":"`+e.people["newcomer"].String()+`"}`)
			statuses <- [2]any{name, got.status}
		}()
	}
	// An unrelated query while they run.
	time.Sleep(100 * time.Millisecond)
	queried := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	var one int
	err = pool.QueryRow(ctx, `SELECT 1`).Scan(&one)
	cancel()
	if err != nil || time.Since(queried) > time.Second {
		t.Fatalf("an unrelated query waited %s: %v", time.Since(queried), err)
	}
	wg.Wait()
	close(statuses)
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("the writes took %s", elapsed)
	}
	for got := range statuses {
		name, status := got[0].(string), got[1].(int)
		want := fiber.StatusForbidden
		if name[:8] == "pgLeader" {
			want = fiber.StatusNoContent
		}
		if status != want {
			t.Errorf("%s: %d, want %d", name, status, want)
		}
	}
	var denied, ok int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE outcome = 'denied' AND actor_basis = 'none'), count(*) FILTER (WHERE outcome = 'ok' AND actor_basis = 'leader') FROM team_audit_records`).Scan(&denied, &ok); err != nil {
		t.Fatal(err)
	}
	if denied != len(outsiders) || ok != len(leaders) {
		t.Fatalf("rows: %d denied, %d ok; want %d and %d", denied, ok, len(outsiders), len(leaders))
	}
	for _, name := range targets {
		if !e.directlyIn(name, "/UYELER/ARGE/WEBLAB") {
			t.Fatalf("%s not added", name)
		}
	}
}

// A write waits for the Team code lock no longer than its timeout: a lock
// held elsewhere (another core) turns it into a 503, not a request that
// hangs with a connection.
func TestTeamDelegationWriteGivesUpOnAHeldLock(t *testing.T) {
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	e := newDelegationEnv(t, delegationSetup{on: true})
	audit := teamaudit.NewPostgres(pool)
	policy := authz.DefaultPolicy()
	policy.TeamDelegation = true
	az := authz.NewAuthorizer(policy)
	delegation := identity.NewTeamDelegation(policy, e.dir, e.dir, identity.TeamDelegationOptions{WriteTimeout: 1500 * time.Millisecond})
	options := identity.Options{TeamCodes: teamcode.NewPostgresLedger(pool), TeamAudit: audit, Delegation: delegation}
	deps := memoryDepsDecidedBy(e.dir, options, az)
	deps.Identity = identity.NewServiceWithOptions(identity.AuditedDirectory(e.dir, audit, nil), e.users, az, options)
	deps.ParseToken = e.keys.Parse()
	e.app = httpx.New(deps)

	holder, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, teamcode.LockName); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := e.add("webLeader", "/UYELER/ARGE/WEBLAB", "newcomer")
	if got.status != fiber.StatusServiceUnavailable || time.Since(start) > 5*time.Second {
		t.Fatalf("status %d after %s (%s)", got.status, time.Since(start), got.body)
	}
	if e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB") {
		t.Fatal("added without the lock")
	}
}
