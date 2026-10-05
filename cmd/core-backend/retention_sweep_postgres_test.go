package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// By hand, inside the container: a dry run counts and changes nothing, an
// apply changes, and a run while another holds the lock does nothing and
// says so. Both runs are recorded as the command's.
func TestRetentionSweepCommandAgainstPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID, ticketID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, end_date) VALUES ($1, 'old', 'YTÜ', 'SKY LAB', $2)`,
		eventID, time.Now().Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_email, guest_phone_number)
		VALUES ($1, $2, 'GUEST', 'Ada', 'ada@example.com', '+905551112233')`, ticketID, eventID); err != nil {
		t.Fatal(err)
	}
	env := retentionEnv(map[string]string{"DATABASE_URL": pool.Config().ConnString(), "RETENTION_SWEEP_MODE": "dry-run"})
	phone := func() string {
		var value string
		if err := pool.QueryRow(ctx, `SELECT guest_phone_number FROM tickets WHERE id = $1`, ticketID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}

	var out bytes.Buffer
	if code := runRetentionSweep(nil, env, &out); code != 0 || phone() == "" ||
		!strings.Contains(out.String(), "dry run, nothing was changed") || !strings.Contains(out.String(), "dry_run") {
		t.Fatalf("dry run: exit %d\n%s", code, out.String())
	}
	out.Reset()
	if code := runRetentionSweep([]string{"--apply", "--rule", "guest_phone"}, env, &out); code != 0 || phone() != "" {
		t.Fatalf("apply: exit %d\n%s", code, out.String())
	}
	for _, value := range []string{"ada@example.com", "Ada", "+90555"} {
		if strings.Contains(out.String(), value) {
			t.Fatalf("output shows %q:\n%s", value, out.String())
		}
	}

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('core-backend retention sweep', 0))`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := runRetentionSweep([]string{"--apply"}, env, &out); code != 3 {
		t.Fatalf("locked: exit %d\n%s", code, out.String())
	}
	var runs string
	if err := pool.QueryRow(ctx, `SELECT string_agg(mode || ':' || triggered_by || ':' || status, ',' ORDER BY started_at) FROM retention_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != "dry-run:cli:ok,apply:cli:ok,apply:cli:skipped_locked" {
		t.Fatalf("runs %q", runs)
	}
}
