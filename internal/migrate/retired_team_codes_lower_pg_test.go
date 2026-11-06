package migrate_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const retiredLowerVersion = "20261030110000"

// A code retired in two cases before the index existed does not stop the
// migration, and neither row is lost: the earliest stays, the others move to
// retired_team_codes_case_duplicates. Afterwards a second case of a code is
// refused. The fingerprint knows the index; down puts the moved rows back.
func TestRetiredTeamCodesLowerMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down := readDown(t, retiredLowerVersion+"_retired_team_codes_lower.down.sql")
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(down)
	exec(`DELETE FROM schema_migrations WHERE version = ` + retiredLowerVersion)
	exec(`INSERT INTO retired_team_codes (code, retired_at, reason) VALUES
		('WEBLAB', now() - interval '2 days', 'renamed'),
		('weblab', now() - interval '1 day', 'deleted'),
		('Weblab', now(), 'deleted'),
		('AIRLAB', now(), 'renamed')`)

	if err := migrate.Apply(ctx, pool); err != nil || !recorded(t, pool, retiredLowerVersion) {
		t.Fatalf("with duplicates: %v", err)
	}
	codes := func(table string) []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT code FROM `+table+` ORDER BY code`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			out = append(out, c)
		}
		return out
	}
	if got := codes("retired_team_codes"); len(got) != 2 || got[0] != "AIRLAB" || got[1] != "WEBLAB" {
		t.Fatalf("kept %v", got)
	}
	if got := codes("retired_team_codes_case_duplicates"); len(got) != 2 || got[0] != "Weblab" || got[1] != "weblab" {
		t.Fatalf("moved %v", got)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO retired_team_codes (code, reason) VALUES ('airlab', 'renamed')`); err == nil {
		t.Fatal("a second case of a retired code was taken")
	}

	exec(`DELETE FROM schema_migrations WHERE version = ` + retiredLowerVersion)
	if err := migrate.Apply(ctx, pool); err != nil || !recorded(t, pool, retiredLowerVersion) {
		t.Fatalf("fingerprint: %v", err)
	}

	exec(down)
	if got := codes("retired_team_codes"); len(got) != 4 {
		t.Fatalf("down kept %v", got)
	}
	exec(`DELETE FROM retired_team_codes WHERE code <> 'WEBLAB' AND lower(code) = 'weblab'`)
	exec(`DELETE FROM schema_migrations WHERE version = ` + retiredLowerVersion)
	if err := migrate.Apply(ctx, pool); err != nil || !recorded(t, pool, retiredLowerVersion) {
		t.Fatalf("again: %v", err)
	}
	if got := codes("retired_team_codes_case_duplicates"); len(got) != 0 {
		t.Fatalf("moved without duplicates %v", got)
	}
}
