package account_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresWorkerPersistsProgressAcrossRestart(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	subjectID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "worker@example.com", FirstName: "Worker"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}

	now := request.NextAttemptAt.Add(time.Minute)
	confirmDeletionProjection(t, store, request, now)
	identity := &uncertainIdentity{}
	config := account.WorkerConfig{
		Now: func() time.Time { return now }, Lease: time.Minute, MaxAttempts: 3,
		AccessBlocker: &accountBlockWriter{},
	}
	if worked, err := account.NewWorker(store, identity, config, noAccountMedia{}).RunOnce(ctx); !worked || err == nil {
		t.Fatalf("first worker worked=%v err=%v", worked, err)
	}

	if worked, err := account.NewWorker(store, identity, config, noAccountMedia{}).RunOnce(ctx); !worked || err != nil {
		t.Fatalf("restarted worker worked=%v err=%v", worked, err)
	}
	completed, err := store.DeletionRequest(ctx, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.ID != request.ID || completed.Status != user.DeletionRequestCompleted || completed.AttemptCount != 2 {
		t.Fatalf("request after restart: %+v", completed)
	}
	var stepCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_deletion_steps WHERE request_id = $1`, request.ID).Scan(&stepCount); err != nil {
		t.Fatal(err)
	}
	if stepCount != 6 {
		t.Fatalf("step count = %d, want 6", stepCount)
	}
}

func TestPostgresServiceErasureKeepsProofAndHonoursTheFence(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	stepCounts := func(requestID uuid.UUID) map[string]string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT step, COALESCE(counts::text, '') FROM account_deletion_steps WHERE request_id=$1`, requestID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var step, counts string
			if err := rows.Scan(&step, &counts); err != nil {
				t.Fatal(err)
			}
			out[step] = counts
		}
		return out
	}

	// One service unavailable: the other two are checkpointed with their
	// counts in the same pass, and the request waits without spending an attempt.
	f := newErasureFixtureWith(t, store)
	f.services[user.DeletionStepEraseCMS].set(answerStatus(http.StatusServiceUnavailable, "60"))
	worker := f.worker()
	if worked, err := f.run(worker); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	proof := stepCounts(f.request.ID)
	if len(proof) != 2 || proof["erase_skymail"] != `{"skymail_rows_erased": 1}` || proof["erase_forms"] != `{"forms_rows_erased": 1}` {
		t.Fatalf("proof after first pass = %v", proof)
	}
	state := f.state()
	if state.Status != user.DeletionRequestPending || state.AttemptCount != 0 || state.LastErrorCode != "erase_cms_failed" ||
		!state.NextAttemptAt.Equal(f.now.Add(time.Minute)) {
		t.Fatalf("deferred request = %+v", state)
	}
	f.now = f.now.Add(time.Minute)
	f.services[user.DeletionStepEraseCMS].set(answerCompleted(map[string]int64{"collection_items_updated": 2}))
	if worked, err := f.run(worker); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := f.state(); state.Status != user.DeletionRequestCompleted {
		t.Fatalf("request = %+v", state)
	}
	if proof := stepCounts(f.request.ID); len(proof) != 3 || proof["erase_cms"] != `{"collection_items_updated": 2}` {
		t.Fatalf("proof = %v", proof)
	}
	if f.services[user.DeletionStepEraseSkyMail].callCount() != 1 || f.services[user.DeletionStepEraseForms].callCount() != 1 {
		t.Fatal("a checkpointed service was called again")
	}

	// The runbook's proof query lists the completed request with each step's
	// time and counts, and nothing that identifies the person.
	rows, err := pool.Query(ctx, erasureProofQuery(t))
	if err != nil {
		t.Fatal(err)
	}
	proofRows := 0
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		for i, value := range values {
			if id, ok := value.([16]byte); ok {
				values[i] = uuid.UUID(id).String()
			}
		}
		line := fmt.Sprint(values...)
		f.assertNoPersonalData(line)
		if strings.Contains(line, f.request.ID.String()) {
			proofRows++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || proofRows != 3 {
		t.Fatalf("proof query rows for the request = %d err=%v", proofRows, err)
	}

	// A rejection goes to manual intervention at once, with its code.
	g := newErasureFixtureWith(t, store)
	g.services[user.DeletionStepEraseForms].set(answerStatus(http.StatusConflict, ""))
	if worked, err := g.run(g.worker()); !worked || err == nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if state := g.state(); state.Status != user.DeletionRequestManualIntervention || state.LastErrorCode != "erase_forms_rejected_409" || state.AttemptCount != 1 {
		t.Fatalf("rejected request = %+v", state)
	}

	// A worker that lost its lease writes no proof and calls nothing more.
	h := newErasureFixtureWith(t, store)
	thieves := make(chan user.DeletionRequest, 1)
	h.services[user.DeletionStepEraseSkyMail].set(func(w http.ResponseWriter, r *http.Request) {
		claimed, ok, err := store.ClaimDeletionRequest(context.Background(), h.now.Add(10*time.Minute), time.Minute)
		if err != nil || !ok || claimed.ID != h.request.ID {
			t.Errorf("thief claim=%+v ok=%v err=%v", claimed, ok, err)
		}
		thieves <- claimed
		answerCompleted(map[string]int64{"recipients_deleted": 1})(w, r)
	})
	if worked, err := h.run(h.worker()); !worked || !errors.Is(err, user.ErrLeaseLost) {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	thief := <-thieves
	if proof := stepCounts(h.request.ID); len(proof) != 0 {
		t.Fatalf("a fenced-out worker wrote proof: %v", proof)
	}
	if h.services[user.DeletionStepEraseCMS].callCount() != 0 || h.services[user.DeletionStepEraseForms].callCount() != 0 {
		t.Fatal("the fenced-out worker went on to call other services")
	}
	if state := h.state(); state.Status != user.DeletionRequestProcessing || state.LeaseToken == nil || thief.LeaseToken == nil || *state.LeaseToken != *thief.LeaseToken {
		t.Fatalf("the new lease was disturbed: %+v", state)
	}
	f.assertNoPersonalData()
	g.assertNoPersonalData(g.state().LastErrorCode)
	h.assertNoPersonalData()
}

func TestPostgresWatchdogGaugesMatchTheFixtures(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	fixtures := []struct {
		status user.DeletionRequestStatus
		age    time.Duration
		code   string
	}{
		{user.DeletionRequestPending, 24 * time.Hour, ""},
		{user.DeletionRequestProcessing, 21 * 24 * time.Hour, "erase_cms_failed"},
		{user.DeletionRequestManualIntervention, 2 * 24 * time.Hour, "erase_forms_rejected_403"},
		{user.DeletionRequestCompleted, 40 * 24 * time.Hour, ""},
	}
	for i, fixture := range fixtures {
		subjectID := uuid.New()
		if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "watchdog" + string(rune('a'+i)) + "@example.test"}); err != nil {
			t.Fatal(err)
		}
		request, err := store.RequestDeletion(ctx, subjectID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET status=$2, created_at=$3, last_error_code=$4 WHERE id=$1`,
			request.ID, fixture.status, now.Add(-fixture.age), fixture.code); err != nil {
			t.Fatal(err)
		}
	}
	gauges := account.NewErasureGauges()
	var events []account.Attention
	watchdog := account.NewWatchdog(store, gauges, account.WatchdogConfig{
		AlertAfter: 480 * time.Hour, Now: func() time.Time { return now },
		Attention: func(event account.Attention) { events = append(events, event) },
	})
	if err := watchdog.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	text := gauges.Prometheus()
	for _, line := range []string{
		"skylab_account_erasure_open_requests 3\n",
		"skylab_account_erasure_overdue_requests 1\n",
		"skylab_account_erasure_manual_intervention_requests 1\n",
		"skylab_account_erasure_oldest_open_age_seconds 1814400\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("gauges lack %q:\n%s", line, text)
		}
	}
	if len(events) != 2 {
		t.Fatalf("attention events = %+v", events)
	}
}

// erasureProofQuery reads the proof query from the runbook, so the documented
// query is the tested one.
func erasureProofQuery(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "account-lifecycle.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(raw), "<!-- erasure-proof-query -->")
	if !ok {
		t.Fatal("runbook has no erasure proof query")
	}
	_, rest, ok = strings.Cut(rest, "```sql\n")
	query, _, closed := strings.Cut(rest, "```")
	if !ok || !closed || strings.Contains(query, "subject_id") {
		t.Fatalf("runbook proof query is malformed or names the subject: %q", query)
	}
	return query
}
