package retention

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// A scrubbed click keeps where it came from, the referer's origin, and
// nothing that could name the person: no user information, path, query or
// fragment. The reduction is its own fixed point, so url_hits_scrub never
// finds a scrubbed row due again.
func TestRefererOriginKeepsSchemeAndHostOnly(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	for raw, want := range map[string]string{
		"https://User:Pw@Example.COM:8443/path/ada?token=x#frag": "https://example.com",
		"https://www.instagram.com/":                             "https://www.instagram.com",
		"http://l.facebook.com/l.php?u=https%3A%2F%2Fskyl.app":   "http://l.facebook.com",
		"android-app://com.google.android.gm/":                   "android-app://com.google.android.gm",
		"https://[2001:db8::1]:8080/x":                           "https://[2001:db8::1]",
		"HTTPS://Mixed.Case.Example":                             "https://mixed.case.example",
		"https://a@b@example.com/":                               "https://example.com",
		"https://":                                               "https://",
		"https://example.com":                                    "https://example.com",
		"ada@example.com":                                        "",
		"//example.com/path":                                     "",
		"not a url":                                              "",
		"":                                                       "",
	} {
		var once, twice string
		if err := pool.QueryRow(ctx, `SELECT `+refererOriginSQL("r")+`, `+refererOriginSQL(refererOriginSQL("r"))+` FROM (SELECT $1::text AS r) v`, raw).
			Scan(&once, &twice); err != nil {
			t.Fatal(err)
		}
		if once != want || twice != once {
			t.Errorf("%q: %q then %q, want %q", raw, once, twice, want)
		}
	}
}

// The daily scrub and its count find the click rows that still hold
// something personal through url_hits_personal_at_idx, whose predicate is
// the rule's own: rows leave the index as they are scrubbed, so a day's work
// reads a day's rows, not every click ever kept. The migration's predicate
// is the one the rule is built from.
func TestURLHitsScrubReadsItsPartialIndex(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	up, err := fs.ReadFile(db.UpSQL, "migrations/"+urlHitsIndexMigration)
	if err != nil {
		t.Fatal(err)
	}
	if want := "CREATE INDEX CONCURRENTLY IF NOT EXISTS url_hits_personal_at_idx ON url_hits (at) WHERE " + hitPersonalSQL("") + ";"; !strings.Contains(string(up), want) {
		t.Fatalf("the migration's index predicate is not the rule's:\n%s\nwant\n%s", up, want)
	}
	var rule Rule
	for _, r := range Rules(Config{Mode: ModeApply, Period: 90 * day}, Schema{}) {
		if r.Name == "url_hits_scrub" {
			rule = r
		}
	}
	batch := "SELECT h.id FROM url_hits h WHERE " + rule.where + " LIMIT $2 FOR UPDATE OF h SKIP LOCKED"
	for name, query := range map[string]string{"count": rule.countSQL(), "batch": batch} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// An empty test table: forbid the sequential scan so the plan shows
		// whether the index can serve the query at all.
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		args := []any{time.Now().Add(-365 * day)}
		if name == "batch" {
			args = append(args, 500)
		}
		rows, err := tx.Query(ctx, "EXPLAIN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		_ = tx.Rollback(ctx)
		if !strings.Contains(plan.String(), "url_hits_personal_at_idx") {
			t.Errorf("%s does not read the index:\n%s", name, plan.String())
		}
	}
}
