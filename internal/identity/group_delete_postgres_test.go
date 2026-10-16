package identity

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// CI uses the repository's disposable Docker fixture. A developer may point
// this one test at their own fresh, disposable native PostgreSQL database;
// never set this to a shared or production database (migrations are applied).
func deletionPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	var pool *pgxpool.Pool
	if url := os.Getenv("CORE_GROUP_DELETE_TEST_POSTGRES_URL"); url != "" {
		var err error
		pool, err = pgxpool.New(context.Background(), url)
		if err != nil {
			t.Fatal("invalid disposable group-deletion database")
		}
		t.Cleanup(pool.Close)
	} else {
		pool = testpostgres.Start(t)
	}
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestGroupDeletePostgresDurableRetirement(t *testing.T) {
	pool := deletionPostgres(t)
	for _, failure := range []string{"timeout", "known refusal", "controlled abort", "second commit cancelled", "audit finish unavailable"} {
		t.Run(failure, func(t *testing.T) {
			dir, g, _, _ := deleteTree(t)
			g.Name = "DELETE" + strings.ToUpper(uuid.NewString()[:8])
			g.Path = "/" + g.Name
			dir.PutGroup(g)
			ledger := teamcode.NewPostgresLedger(pool)
			audit := teamaudit.NewPostgres(pool)
			p := deletePrincipal()
			ctx, cancel := context.WithCancel(deleteContext(p))
			defer cancel()
			failing := &deleteFailureDirectory{Directory: dir}
			switch failure {
			case "timeout":
				failing.fail = context.DeadlineExceeded
			case "known refusal":
				failing.fail = ErrForbidden
			case "controlled abort":
				failing.panicDelete = true
			case "second commit cancelled":
				failing.before = cancel
			}
			var writer teamaudit.Writer = audit
			if failure == "audit finish unavailable" {
				writer = deleteFinishUnavailable{Writer: audit}
			}
			svc := deleteService(failing, ledger, writer)
			var err error
			var aborted bool
			func() {
				defer func() {
					if recover() != nil {
						aborted = true
					}
				}()
				err = svc.DeleteGroup(ctx, p, g.ID)
			}()
			if failure == "controlled abort" && !aborted {
				t.Fatal("abort was not reached")
			}
			if failure != "controlled abort" && failure != "audit finish unavailable" && err == nil {
				t.Fatal("failure was hidden")
			}
			// This read uses a different connection after the second transaction
			// ended/rolled back: the first transaction's tombstone is durable.
			var reason string
			if err := pool.QueryRow(context.Background(), `SELECT reason FROM retired_team_codes WHERE code=$1`, g.Name).Scan(&reason); err != nil || reason != teamcode.ReasonDeleted {
				t.Fatalf("retirement lost: %q %v", reason, err)
			}
			var outcome string
			if err := pool.QueryRow(context.Background(), `SELECT outcome FROM team_audit_records WHERE group_id=$1 ORDER BY occurred_at DESC LIMIT 1`, g.ID).Scan(&outcome); err != nil {
				t.Fatal(err)
			}
			want := "pending"
			if failure == "known refusal" {
				want = "failed"
			}
			if outcome != want {
				t.Fatalf("audit outcome %q want %q", outcome, want)
			}
			if failure == "second commit cancelled" || failure == "audit finish unavailable" {
				if _, err := dir.GetGroup(context.Background(), g.ID); !errors.Is(err, ErrNotFound) {
					t.Fatal("DELETE did not complete before final failure")
				}
			} else {
				failing.fail = nil
				failing.panicDelete = false
				if err := deleteService(failing, ledger, audit).DeleteGroup(deleteContext(p), p, g.ID); err != nil {
					t.Fatalf("retired code blocked retry: %v", err)
				}
			}
			if _, err := deleteService(dir, ledger, audit).CreateGroup(deleteContext(p), p, "", g.Name); err == nil {
				t.Fatal("deleted code was reused after failure")
			}
		})
	}
	t.Run("live owner records block deletion", func(t *testing.T) {
		dir, g, _, _ := deleteTree(t)
		g.Name = "REF" + strings.ToUpper(uuid.NewString()[:8])
		g.Path = "/" + g.Name
		dir.PutGroup(g)
		if _, err := pool.Exec(context.Background(), `INSERT INTO events(id,name,location,owner_team) VALUES($1,'Example','Test',$2)`, uuid.New(), strings.ToLower(g.Name)); err != nil {
			t.Fatal(err)
		}
		p := deletePrincipal()
		err := deleteService(dir, teamcode.NewPostgresLedger(pool), teamaudit.NewPostgres(pool)).DeleteGroup(deleteContext(p), p, g.ID)
		if deleteCode(err) != "code_in_use" {
			t.Fatalf("live owner reference ignored: %v", err)
		}
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM retired_team_codes WHERE code=$1`, g.Name).Scan(&n); err != nil || n != 0 {
			t.Fatal("refused deletion retired code")
		}
	})
	t.Run("first commit failure never calls DELETE", func(t *testing.T) {
		dir, g, _, _ := deleteTree(t)
		g.Name = "FIRST" + strings.ToUpper(uuid.NewString()[:8])
		g.Path = "/" + g.Name
		dir.PutGroup(g)
		called := false
		remote := &deleteFailureDirectory{Directory: dir, before: func() { called = true }}
		ledger := deleteCancelFirstCommit{Ledger: teamcode.NewPostgresLedger(pool)}
		p := deletePrincipal()
		if err := deleteService(remote, ledger, teamaudit.NewPostgres(pool)).DeleteGroup(deleteContext(p), p, g.ID); err == nil {
			t.Fatal("first commit failure was hidden")
		}
		if called {
			t.Fatal("DELETE ran without confirmed first commit")
		}
		if _, err := dir.GetGroup(context.Background(), g.ID); err != nil {
			t.Fatal("group removed after first commit failure")
		}
	})
	for _, kind := range []string{"certificate", "template", "binding"} {
		t.Run("live reference "+kind, func(t *testing.T) {
			dir, g, _, _ := deleteTree(t)
			g.Name = "REF" + strings.ToUpper(uuid.NewString()[:8])
			g.Path = "/" + g.Name
			dir.PutGroup(g)
			ctx := context.Background()
			eventID, ticketID, templateID := uuid.New(), uuid.New(), uuid.New()
			var err error
			switch kind {
			case "certificate":
				_, err = pool.Exec(ctx, `INSERT INTO events(id,name,location,owner_team) VALUES($1,'Example','Test','')`, eventID)
				if err == nil {
					_, err = pool.Exec(ctx, `INSERT INTO tickets(id,event_id,ticket_type,guest_email) VALUES($1,$2,'GUEST','example@example.test')`, ticketID, eventID)
				}
				if err == nil {
					_, err = pool.Exec(ctx, `INSERT INTO certificates(id,event_id,ticket_id,serial,recipient_name,recipient_email,event_name,owner_team,verify_url) VALUES($1,$2,$3,$4,'Example','example@example.test','Example',$5,'https://example.test')`, uuid.New(), eventID, ticketID, uuid.NewString(), strings.ToLower(g.Name))
				}
			case "template":
				_, err = pool.Exec(ctx, `INSERT INTO certificate_templates(id,name,owner_team) VALUES($1,'Example',$2)`, templateID, strings.ToLower(g.Name))
			case "binding":
				_, err = pool.Exec(ctx, `INSERT INTO certificate_templates(id,name,owner_team) VALUES($1,'Example','')`, templateID)
				if err == nil {
					_, err = pool.Exec(ctx, `INSERT INTO certificate_template_bindings(id,scope,scope_key,template_id) VALUES($1,'ownerTeam',$2,$3)`, uuid.New(), strings.ToLower(g.Name), templateID)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			p := deletePrincipal()
			if err := deleteService(dir, teamcode.NewPostgresLedger(pool), teamaudit.NewPostgres(pool)).DeleteGroup(deleteContext(p), p, g.ID); deleteCode(err) != "code_in_use" {
				t.Fatalf("%s reference ignored: %v", kind, err)
			}
		})
	}

}

type deleteFinishUnavailable struct{ teamaudit.Writer }

func (deleteFinishUnavailable) Finish(context.Context, uuid.UUID, teamaudit.Outcome, string, *teamaudit.State) error {
	return teamaudit.ErrUnavailable
}

// Cancel after Retire, immediately before the real pgx transaction Commit.
type deleteCancelFirstCommit struct{ teamcode.Ledger }

func (l deleteCancelFirstCommit) Locked(ctx context.Context, f func(context.Context, teamcode.Records) error) error {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	return l.Ledger.Locked(child, func(inner context.Context, r teamcode.Records) error { err := f(inner, r); cancel(); return err })
}
