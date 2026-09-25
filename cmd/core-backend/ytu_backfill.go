package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// ytuBackfillCommandName runs the one-time YTÜ profile backfill instead of
// the server: `core-backend backfill-ytu-profile [-apply]`. It reads the
// same environment as the server, so it runs inside the core container.
const ytuBackfillCommandName = "backfill-ytu-profile"

// runYTUBackfill wires the backfill to the database and to Keycloak through
// core's service account (read-only: it lists users). It prints counts and
// department values only, never a person or a configuration value.
func runYTUBackfill(args []string, getenv func(string) string, out io.Writer) int {
	var missing []string
	for _, name := range []string{"DATABASE_URL", "KEYCLOAK_URL", "KEYCLOAK_CLIENT_ID", "KEYCLOAK_CLIENT_SECRET"} {
		if strings.TrimSpace(getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if getenv("KEYCLOAK_REALM") == "" && !strings.Contains(getenv("KEYCLOAK_URL"), "/realms/") {
		missing = append(missing, "KEYCLOAK_REALM")
	}
	if len(missing) > 0 {
		fmt.Fprintf(out, "%s needs %s\n", ytuBackfillCommandName, strings.Join(missing, ", "))
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(out, "database: cannot open the connection pool")
		return 1
	}
	defer pool.Close()
	keycloak := identity.NewKeycloak(identity.KeycloakConfig{
		URL:          getenv("KEYCLOAK_URL"),
		Realm:        getenv("KEYCLOAK_REALM"),
		ClientID:     getenv("KEYCLOAK_CLIENT_ID"),
		ClientSecret: getenv("KEYCLOAK_CLIENT_SECRET"),
	})
	return ytuBackfillCommand(ctx, args, out, keycloak.YTUAttributes, user.NewPostgresStore(pool))
}

func ytuBackfillCommand(ctx context.Context, args []string, out io.Writer, read func(context.Context) ([]user.YTUAttributes, error), store user.Store) int {
	flags := flag.NewFlagSet(ytuBackfillCommandName, flag.ContinueOnError)
	flags.SetOutput(out)
	apply := flags.Bool("apply", false, "write the changes; without it the command only counts them")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	accounts, err := read(ctx)
	if err != nil {
		fmt.Fprintf(out, "keycloak: %v\n", err)
		return 1
	}
	report, err := user.BackfillYTU(ctx, store, accounts, *apply)
	if err != nil {
		fmt.Fprintf(out, "core: %v\n", err)
		return 1
	}
	if *apply {
		fmt.Fprintln(out, "YTÜ profile backfill")
		fmt.Fprintf(out, "records updated: %d\n", report.Updated)
	} else {
		fmt.Fprintln(out, "YTÜ profile backfill, dry run (add -apply to write)")
		fmt.Fprintf(out, "records to update: %d\n", report.Updated)
	}
	fmt.Fprintf(out, "accounts with the YTÜ university: %d\n", report.Accounts)
	fmt.Fprintf(out, "already in step: %d\n", report.Unchanged)
	fmt.Fprintf(out, "no core record yet (filled on first request): %d\n", report.NoRecord)
	fmt.Fprintf(out, "pending deletion or anonymized (left alone): %d\n", report.Blocked)
	if len(report.Unresolved) > 0 {
		fmt.Fprintln(out, "department values with no faculty:")
		values := make([]string, 0, len(report.Unresolved))
		for v := range report.Unresolved {
			values = append(values, v)
		}
		sort.Strings(values)
		for _, v := range values {
			fmt.Fprintf(out, "  %q: %d\n", v, report.Unresolved[v])
		}
	}
	return 0
}
