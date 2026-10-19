// Package testpostgres provides a disposable PostgreSQL fixture for integration tests.
package testpostgres

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultImage is the PostgreSQL the fixture starts: the major version
// deploy/compose.yaml runs locally.
const DefaultImage = "postgres:17-alpine"

// ImageEnv names the variable that replaces DefaultImage. CI sets it to the
// major version production runs (18), so the tests run against that.
const ImageEnv = "CORE_TEST_POSTGRES_IMAGE"

// ServerEnv names a running PostgreSQL server, as a URL whose user may
// create databases (postgres://postgres:postgres@127.0.0.1:5432/postgres).
// When it is set, Start gives each test a new database of its own on that
// server instead of a container of its own: CI runs one server per job, so a
// test pays for a CREATE DATABASE instead of a container start. Unset (the
// default), every test gets its own container.
const ServerEnv = "CORE_TEST_POSTGRES_URL"

func image() string {
	if image := os.Getenv(ImageEnv); image != "" {
		return image
	}
	return DefaultImage
}

// Start launches a disposable PostgreSQL container and registers all cleanup
// with t. Tests are skipped when Docker is unavailable. With ServerEnv set it
// creates a disposable database on that server instead (see ServerEnv).
func Start(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if server := os.Getenv(ServerEnv); server != "" {
		return startDatabase(t, server)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("core-postgres-test-%d", time.Now().UnixNano())
	// The image declares a VOLUME (PostgreSQL 17: /var/lib/postgresql/data,
	// 18: /var/lib/postgresql), so a container would leave an anonymous
	// volume behind: `--rm` only removes it when the container exits on its
	// own, not when Cleanup removes it. A tmpfs on each of the two paths
	// covers whichever the image declares, so no volume is created and the
	// data stays in memory, which also makes the tests faster. PGDATA is
	// spelled out because PostgreSQL 18's image keeps its data elsewhere by
	// default (/var/lib/postgresql/18/docker).
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"--tmpfs", "/var/lib/postgresql",
		"--tmpfs", "/var/lib/postgresql/data",
		"-e", "PGDATA=/var/lib/postgresql/data",
		"-e", "POSTGRES_PASSWORD=postgres",
		"-e", "POSTGRES_DB=coretest",
		"-p", "127.0.0.1::5432",
		image(),
	)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Skipf("docker run postgres: %v %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "-v", name).Run()
	})

	portCtx, cancelPort := context.WithTimeout(context.Background(), 30*time.Second)
	hostport, err := waitForPublishedPort(portCtx, 100*time.Millisecond, func() ([]byte, error) {
		return exec.Command("docker", "port", name, "5432/tcp").CombinedOutput()
	})
	cancelPort()
	if err != nil {
		t.Fatal(err)
	}

	url := fmt.Sprintf("postgres://postgres:postgres@%s/coretest?sslmode=disable", hostport)
	deadline := time.Now().Add(30 * time.Second)
	for {
		pool, poolErr := pgxpool.New(context.Background(), url)
		if poolErr == nil {
			poolErr = pool.Ping(context.Background())
		}
		if poolErr == nil {
			t.Cleanup(pool.Close)
			return pool
		}
		if pool != nil {
			pool.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres never became ready: %v", poolErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// databases numbers the databases this test process created, so that their
// names, with the process id, are unique on a server several test processes
// share.
var databases atomic.Int64

// startDatabase creates a new database on the server at serverURL and opens
// it; Cleanup closes the pool and drops the database, ending any connection
// the test left open. A server that cannot be reached fails the test: the
// environment asked for it.
func startDatabase(t testing.TB, serverURL string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("coretest_%d_%d", os.Getpid(), databases.Add(1))
	admin := func(sql string) error {
		conn, err := pgx.Connect(ctx, serverURL)
		if err != nil {
			return fmt.Errorf("connect to %s: %w", ServerEnv, err)
		}
		defer conn.Close(ctx)
		_, err = conn.Exec(ctx, sql)
		return err
	}
	if err := admin("CREATE DATABASE " + pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if err := admin("DROP DATABASE IF EXISTS " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})

	dsn, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("%s: %v", ServerEnv, err)
	}
	dsn.Path = "/" + name
	pool, err := pgxpool.New(ctx, dsn.String())
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// WaitForBlockedQuery waits until another connection is blocked on a lock
// while executing SQL containing fragment. It makes database race tests
// deterministic without sleeps tied to machine speed.
func WaitForBlockedQuery(t testing.TB, pool *pgxpool.Pool, fragment string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked bool
		err := pool.QueryRow(context.Background(), `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE datname = current_database()
				  AND pid <> pg_backend_pid()
				  AND wait_event_type = 'Lock'
				  AND query LIKE '%' || $1 || '%'
			)
		`, fragment).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("query containing %q did not block before timeout", fragment)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPublishedPort(ctx context.Context, retryInterval time.Duration, lookup func() ([]byte, error)) (string, error) {
	var lastErr error
	for {
		out, err := lookup()
		if err == nil {
			if hostport := parsePublishedPort(out); hostport != "" {
				return hostport, nil
			}
			lastErr = fmt.Errorf("docker port returned empty output")
		} else {
			detail := strings.TrimSpace(string(out))
			if detail == "" {
				lastErr = fmt.Errorf("docker port: %w", err)
			} else {
				lastErr = fmt.Errorf("docker port: %w: %s", err, detail)
			}
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("docker port was not published before timeout: %v: %w", lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func parsePublishedPort(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		hostport := strings.TrimSpace(line)
		if hostport == "" {
			continue
		}
		if i := strings.LastIndex(hostport, "://"); i >= 0 {
			hostport = hostport[i+3:]
		}
		if hostport != "" {
			return hostport
		}
	}
	return ""
}
