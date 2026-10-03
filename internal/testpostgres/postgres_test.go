package testpostgres

import (
	"context"
	"testing"
	"time"
)

func TestImageIsTheDefaultUnlessTheEnvironmentNamesOne(t *testing.T) {
	t.Setenv(ImageEnv, "")
	if got := image(); got != DefaultImage {
		t.Fatalf("image %q, want the default %q", got, DefaultImage)
	}
	t.Setenv(ImageEnv, "postgres:18-alpine")
	if got := image(); got != "postgres:18-alpine" {
		t.Fatalf("image %q, want the one the environment names", got)
	}
}

// The fixture keeps PostgreSQL's data on its tmpfs whatever the major version,
// so PostgreSQL 18, whose image defaults to another directory, starts and works.
func TestStartRunsPostgreSQL18WhenTheEnvironmentAsksForIt(t *testing.T) {
	t.Setenv(ImageEnv, "postgres:18-alpine")
	pool := Start(t)

	var version int
	if err := pool.QueryRow(context.Background(), `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 180000 || version >= 190000 {
		t.Fatalf("server_version_num %d, want PostgreSQL 18", version)
	}
}

func TestWaitForPublishedPortRetriesSuccessfulEmptyOutput(t *testing.T) {
	t.Parallel()

	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	hostport, err := waitForPublishedPort(ctx, 0, func() ([]byte, error) {
		calls++
		if calls < 3 {
			return []byte("\n"), nil
		}
		return []byte("127.0.0.1:49152\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if hostport != "127.0.0.1:49152" || calls != 3 {
		t.Fatalf("hostport=%q calls=%d", hostport, calls)
	}
}
