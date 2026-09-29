package media_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The address lookup finds a Media by its lookup key in the database
// (media_lookup_key, migration 20260929180000), and the lookup's addresses
// are turned into keys in Go (lookupKeyOf): the two must agree, and a whole
// batch is read through the index.
func TestLookupKeyInTheDatabase(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}

	t.Run("the same key as Go's", func(t *testing.T) {
		id := uuid.NewString()
		claim := strings.ReplaceAll(uuid.NewString(), "-", "")
		copyKey, ok := media.FaststartCopyKey("videos/"+id+".mp4", uuid.New())
		if !ok {
			t.Fatal("no faststart copy key")
		}
		for _, key := range []string{
			copyKey,
			"videos/" + id + ".fs." + claim + ".mp4",
			"videos/" + id + ".mp4",
			"videos/" + id + ".fs." + claim[:31] + ".mp4",
			"videos/" + id + ".fs." + strings.ToUpper(claim) + ".mp4",
			"videos/" + id + ".fs." + claim + ".mp4/card.jpg",
			"videos/not-a-uuid.fs." + claim + ".mp4",
			"xvideos/" + id + ".fs." + claim + ".mp4",
			"images/" + id, "images/" + id + ".svg", "files/" + id, "private/" + id, "pending/scan/" + id,
			"https://cdn.yildizskylab.com/images/" + id, "",
		} {
			var inDatabase string
			if err := pool.QueryRow(ctx, `SELECT media_lookup_key($1)`, key).Scan(&inDatabase); err != nil {
				t.Fatal(err)
			}
			if inGo := media.LookupKeyOf(key); inGo != inDatabase {
				t.Errorf("%q: Go %q, the database %q", key, inGo, inDatabase)
			}
		}
		if got := media.LookupKeyOf(copyKey); got != "videos/"+id+".mp4" {
			t.Fatalf("a faststart copy's lookup key is %q", got)
		}
	})

	t.Run("a batch through the index", func(t *testing.T) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		// An empty table is read faster whole; forbid that, as a table of
		// many Media would.
		if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		rows, err := conn.Query(ctx, `EXPLAIN `+media.LookUpKeysSQL,
			[]string{"images/" + uuid.NewString(), "videos/" + uuid.NewString() + ".mp4"}, "cms")
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
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), "media_lookup_key_idx") {
			t.Fatalf("plan does not use the index:\n%s", plan.String())
		}
	})
}
