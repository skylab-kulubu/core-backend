package certificate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// Once a job's lease runs out and another claim takes it, the earlier holder
// can neither renew, complete nor fail it; the new holder can (ticket 25).
func TestPostgresJobLeaseFencesTheEarlierHolder(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(48 * time.Hour)
	ev, err := event.NewPostgresStore(pool).Create(ctx, event.Event{Name: "kira", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	ticketID, batchID, jobID := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', 'ada@example.com')`,
			[]any{ticketID, ev.ID}},
		{`INSERT INTO certificate_batches (id, event_id, template_version_id, template_source, reason)
			VALUES ($1, $2, '89c0bfe1-5be5-4ddb-8686-4865f51e3401', 'clubDefault', 'reissue')`, []any{batchID, ev.ID}},
		{`INSERT INTO certificate_jobs (id, batch_id, event_id, ticket_id, status) VALUES ($1, $2, $3, $4, 'queued')`,
			[]any{jobID, batchID, ev.ID, ticketID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}

	store := certificate.NewPostgresStore(pool)
	claim := func() certificate.Job {
		t.Helper()
		claimed, err := store.ClaimJobs(ctx, 10, time.Minute)
		if err != nil || len(claimed) != 1 || claimed[0].ID != jobID || claimed[0].LeaseToken == uuid.Nil {
			t.Fatalf("claim %+v %v", claimed, err)
		}
		return claimed[0]
	}
	earlier := claim()
	if err := store.RenewJobLease(ctx, jobID, earlier.LeaseToken, time.Minute); err != nil {
		t.Fatalf("holder renews: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE certificate_jobs SET lease_until = now() - interval '1 second' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	later := claim()
	if later.LeaseToken == earlier.LeaseToken {
		t.Fatal("the second claim kept the first claim's lease token")
	}

	if err := store.RenewJobLease(ctx, jobID, earlier.LeaseToken, time.Minute); !errors.Is(err, certificate.ErrLeaseLost) {
		t.Fatalf("earlier holder renews: %v", err)
	}
	if err := store.CompleteJob(ctx, jobID, earlier.LeaseToken, uuid.New()); !errors.Is(err, certificate.ErrLeaseLost) {
		t.Fatalf("earlier holder completes: %v", err)
	}
	if err := store.FailJob(ctx, jobID, earlier.LeaseToken, "late", time.Now(), false); !errors.Is(err, certificate.ErrLeaseLost) {
		t.Fatalf("earlier holder fails the job: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM certificate_jobs WHERE id = $1`, jobID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("after the earlier holder: %q %v", status, err)
	}

	if err := store.FailJob(ctx, jobID, later.LeaseToken, "retry", time.Now().Add(-time.Second), false); err != nil {
		t.Fatalf("holder fails the job: %v", err)
	}
	if err := store.CompleteJob(ctx, jobID, later.LeaseToken, uuid.New()); !errors.Is(err, certificate.ErrLeaseLost) {
		t.Fatalf("a queued job completes under its old lease: %v", err)
	}
	last := claim()
	certificateID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, pdf)
		VALUES ($1, $2, $3, 'kira-serial', 'Ada Lovelace', 'ada@example.com', 'kira', 'WEBLAB', 'https://skyl.app/c/kira-serial', '')`, certificateID, ev.ID, ticketID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteJob(ctx, jobID, last.LeaseToken, certificateID); err != nil {
		t.Fatalf("holder completes: %v", err)
	}
	var token *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT status, lease_token FROM certificate_jobs WHERE id = $1`, jobID).Scan(&status, &token); err != nil || status != "issued" || token != nil {
		t.Fatalf("completed job %q %v %v", status, token, err)
	}
}
