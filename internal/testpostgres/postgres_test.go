package testpostgres

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	t.Setenv(ServerEnv, "")
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

// On a server the environment names, each Start is a database of its own,
// and the test's Cleanup drops it.
func TestStartGivesEachTestADatabaseOfItsOwnOnTheNamedServer(t *testing.T) {
	t.Setenv(ServerEnv, "")
	server := Start(t)
	dsn, err := url.Parse(server.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	dsn.Path = "/postgres"
	t.Setenv(ServerEnv, dsn.String())

	ctx := context.Background()
	var names []string
	t.Run("two tests", func(t *testing.T) {
		for range 2 {
			pool := Start(t)
			var name string
			if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(name, "coretest_") || !strings.Contains(pool.Config().ConnString(), "/"+name) {
				t.Fatalf("database %q, connection string %q", name, pool.Config().ConnString())
			}
			names = append(names, name)
		}
		if names[0] == names[1] {
			t.Fatalf("both tests got the database %q", names[0])
		}
		// A connection the test leaves open does not keep its database.
		if _, err := pgx.Connect(ctx, Start(t).Config().ConnString()); err != nil {
			t.Fatal(err)
		}
	})

	var left int
	if err := server.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname LIKE 'coretest\_%'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d test databases left after the tests' cleanup", left)
	}
}
