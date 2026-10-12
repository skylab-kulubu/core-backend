package handlers

import (
	"context"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// Over the real table: every Group write of Privileged leaves one ok row;
// with the database gone every write answers 503 audit_unavailable and
// none reaches the directory.
func TestGroupWritesOverThePostgresAuditRecord(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f := auditTree()
	app := auditedApp(t, ykIdent(), f.dir, teamaudit.NewPostgres(pool))
	writes := auditWrites(f)
	for _, w := range writes {
		if resp, body := auditRequest(t, app, w.method, w.target, w.body); resp.StatusCode != w.want {
			t.Fatalf("%s: status %d %s", w.name, resp.StatusCode, body)
		}
	}
	var rows, ok int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE outcome = 'ok' AND actor_basis = 'privileged' AND correlation_id <> '') FROM team_audit_records`).Scan(&rows, &ok); err != nil {
		t.Fatal(err)
	}
	if rows != len(writes) || ok != len(writes) {
		t.Fatalf("%d rows, %d ok, for %d writes", rows, ok, len(writes))
	}

	pool.Close()
	f = auditTree()
	app = auditedApp(t, ykIdent(), f.dir, teamaudit.NewPostgres(pool))
	for _, w := range auditWrites(f) {
		if resp, body := auditRequest(t, app, w.method, w.target, w.body); resp.StatusCode != fiber.StatusServiceUnavailable {
			t.Fatalf("%s with the database gone: status %d %s", w.name, resp.StatusCode, body)
		}
	}
	if ops := writeOps(f.dir); len(ops) != 0 {
		t.Fatalf("writes reached the directory: %v", ops)
	}
}
