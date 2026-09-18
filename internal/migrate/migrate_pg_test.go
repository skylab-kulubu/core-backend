package migrate_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

func TestApplyAddsMissingExtraFormURLsOnBrownfield(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE events DROP COLUMN extra_form_urls`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`)
	if err == nil {
		t.Fatal("expected missing column")
	}
	msg := err.Error()
	if !strings.Contains(msg, "extra_form_urls") || !strings.Contains(msg, "42703") {
		t.Fatalf("want SQLSTATE 42703 extra_form_urls, got %v", err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

func postgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("core-migrate-%d", time.Now().UnixNano())
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=postgres",
		"-e", "POSTGRES_DB=coretest",
		"-p", "127.0.0.1::5432",
		"postgres:17-alpine",
	)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Skipf("docker run postgres: %v %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	portOut, err := exec.Command("docker", "port", name, "5432/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("docker port: %v %s", err, portOut)
	}
	addr := strings.TrimSpace(string(portOut))
	parts := strings.Split(addr, "\n")[0]
	hostport := parts
	if i := strings.LastIndex(parts, "://"); i >= 0 {
		hostport = parts[i+3:]
	}

	url := fmt.Sprintf("postgres://postgres:postgres@%s/coretest?sslmode=disable", hostport)
	deadline := time.Now().Add(30 * time.Second)
	var pool *pgxpool.Pool
	for {
		pool, err = pgxpool.New(context.Background(), url)
		if err == nil {
			err = pool.Ping(context.Background())
		}
		if err == nil {
			t.Cleanup(pool.Close)
			return pool
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres never became ready: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestApplyFreshThenIdempotent(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no recorded versions")
	}
	if _, err := pool.Exec(ctx, `SELECT extra_form_urls FROM events`); err != nil {
		t.Fatal(err)
	}
}
