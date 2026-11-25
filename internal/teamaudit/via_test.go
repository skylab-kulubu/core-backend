package teamaudit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// viaWriter is a writer and reader of the record: the memory one or the
// Postgres one.
type viaWriter interface {
	teamaudit.Writer
	teamaudit.Reader
}

// A row's way comes from the context the write runs with (WithVia), on a
// pending row and a denied one alike; a Record that names its own keeps
// it; without one the row has none, as every row before the column; a way
// the table does not know stops the write before it is made. The read
// answers each row's way.
func testVia(t *testing.T, w viaWriter) {
	t.Helper()
	ctx := context.Background()
	if got := teamaudit.ViaFrom(ctx); got != "" {
		t.Fatalf("a bare context carries %q", got)
	}
	actor := uuid.New()
	direct, err := w.Begin(ctx, memberAdd(actor, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	invited, err := w.Begin(teamaudit.WithVia(ctx, teamaudit.ViaInvitation), memberAdd(actor, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Deny(teamaudit.WithVia(ctx, teamaudit.ViaAccessRequest), memberAdd(actor, uuid.New()), "forbidden"); err != nil {
		t.Fatal(err)
	}
	own := memberAdd(actor, uuid.New())
	own.Action, own.Via = teamaudit.ActionMemberRemove, teamaudit.ViaAccessReview
	reviewed, err := w.Begin(teamaudit.WithVia(ctx, teamaudit.ViaInvitation), own)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{direct, invited, reviewed} {
		if err := w.Finish(teamaudit.WithVia(ctx, teamaudit.ViaAccessRequest), id, teamaudit.OutcomeOK, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Begin(teamaudit.WithVia(ctx, "import"), memberAdd(actor, uuid.New())); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("an unknown way from the context: %v", err)
	}
	bad := memberAdd(actor, uuid.New())
	bad.Via = "import"
	if err := w.Deny(ctx, bad, "forbidden"); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("an unknown way of the record: %v", err)
	}

	rows, _, err := w.Records(ctx, teamaudit.Query{ActorID: actor, Limit: teamaudit.MaxPage})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]teamaudit.Via{}
	var denied teamaudit.Via
	for _, r := range rows {
		got[r.ID] = r.Via
		if r.Outcome == teamaudit.OutcomeDenied {
			denied = r.Via
		}
	}
	if len(rows) != 4 || got[direct] != "" || got[invited] != teamaudit.ViaInvitation || got[reviewed] != teamaudit.ViaAccessReview || denied != teamaudit.ViaAccessRequest {
		t.Fatalf("rows %d, ways %v, denied %q", len(rows), got, denied)
	}
}

func TestMemoryVia(t *testing.T) {
	t.Parallel()
	m := teamaudit.NewMemory()
	start := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	n := 0
	m.SetClock(func() time.Time { n++; return start.Add(time.Duration(n) * time.Second) })
	testVia(t, m)
}

func TestPostgresVia(t *testing.T) {
	pool := migrated(t)
	testVia(t, teamaudit.NewPostgres(pool))

	ctx := context.Background()
	insert := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome, via)
		VALUES ($1, now(), gen_random_uuid(), 'privileged', 'member.add', gen_random_uuid(), '/UYELER', gen_random_uuid(), 'pending', $2)`
	if _, err := pool.Exec(ctx, insert, uuid.New(), "import"); err == nil {
		t.Fatal("the table took an unknown way")
	}
	if _, err := pool.Exec(ctx, insert, uuid.New(), ""); err == nil {
		t.Fatal("the table took an empty way")
	}
	// The way is written with the row: neither a pending row nor a finished
	// one changes it.
	id := uuid.New()
	if _, err := pool.Exec(ctx, insert, id, "invitation"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE team_audit_records SET outcome = 'ok', via = 'access_request' WHERE id = $1`, id); err == nil {
		t.Fatal("finishing a row changed its way")
	}
	if _, err := pool.Exec(ctx, `UPDATE team_audit_records SET outcome = 'ok' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE team_audit_records SET via = NULL WHERE id = $1`, id); err == nil {
		t.Fatal("a finished row's way changed")
	}
}
