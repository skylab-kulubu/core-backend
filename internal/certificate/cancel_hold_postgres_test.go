package certificate_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The claim leaves a cancelled Event's jobs waiting, untouched, and takes
// them again once the Event is published again (ticket 04; undoing is
// ticket 15).
func TestPostgresClaimHoldsACancelledEventsJobs(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	events := event.NewPostgresStore(pool)
	start := time.Now().Add(48 * time.Hour)
	job := func(name string) (uuid.UUID, uuid.UUID) {
		ev, err := events.Create(ctx, event.Event{Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start})
		if err != nil {
			t.Fatal(err)
		}
		ticketID, batchID, jobID := uuid.New(), uuid.New(), uuid.New()
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', $3)`,
				[]any{ticketID, ev.ID, name + "@example.com"}},
			{`INSERT INTO certificate_batches (id, event_id, template_version_id, template_source, reason)
				VALUES ($1, $2, '89c0bfe1-5be5-4ddb-8686-4865f51e3401', 'clubDefault', 'manual')`, []any{batchID, ev.ID}},
			{`INSERT INTO certificate_jobs (id, batch_id, event_id, ticket_id, status) VALUES ($1, $2, $3, $4, 'queued')`,
				[]any{jobID, batchID, ev.ID, ticketID}},
		} {
			if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
		return ev.ID, jobID
	}
	cancelled, held := job("iptal")
	_, open := job("acik")
	if changed, err := events.Cancel(ctx, cancelled, nil, ""); err != nil || !changed {
		t.Fatalf("cancel %v %v", changed, err)
	}

	store := certificate.NewPostgresStore(pool)
	claimed, err := store.ClaimJobs(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != open {
		t.Fatalf("claimed %+v", claimed)
	}
	var status string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status, attempt_count FROM certificate_jobs WHERE id = $1`, held).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || attempts != 0 {
		t.Fatalf("held job %s %d", status, attempts)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET status = 'published' WHERE id = $1`, cancelled); err != nil {
		t.Fatal(err)
	}
	if claimed, err = store.ClaimJobs(ctx, 10, time.Minute); err != nil || len(claimed) != 1 || claimed[0].ID != held {
		t.Fatalf("after the Event is back %+v %v", claimed, err)
	}
}
