// Package testpostgres provides a disposable PostgreSQL fixture for integration tests.
package testpostgres

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Start launches a disposable PostgreSQL container and registers all cleanup
// with t. Tests are skipped when Docker is unavailable.
func Start(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("core-postgres-test-%d", time.Now().UnixNano())
	// The image declares its data directory as a VOLUME, so every container
	// would leave an anonymous volume behind: `--rm` only removes it when the
	// container exits on its own, not when Cleanup removes it. Keeping the
	// data in tmpfs creates no volume at all and makes the tests faster.
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"--tmpfs", "/var/lib/postgresql/data",
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
