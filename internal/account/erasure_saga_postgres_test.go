package account_test

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The subtests share one database; each leaves its request completed, so the
// next one's worker claims only its own.
func TestPostgresErasureSaga(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)

	t.Run("waits for every service before anonymizing or deleting", func(t *testing.T) {
		testErasureSagaWaitsForEveryService(t, store)
	})

	t.Run("resumes after manual intervention without repeating a step", func(t *testing.T) {
		f := newSagaFixture(t, store)
		f.answer(user.DeletionStepEraseForms, answerStatus(http.StatusForbidden, ""))
		worker := f.saga()
		if worked, err := f.run(worker); !worked || err == nil {
			t.Fatalf("worked=%v err=%v", worked, err)
		}
		if state := f.state(); state.Status != user.DeletionRequestManualIntervention || state.LastErrorCode != "erase_forms_rejected_403" {
			t.Fatalf("rejected request = %+v", state)
		}
		if f.events.count("anonymize_core") != 0 || f.events.count("delete_identity") != 0 {
			t.Fatalf("a step after the services ran: %v", f.events.list())
		}
		if row := f.core(); row.Email != sagaPersonal || row.SchoolEmail != sagaSchool {
			t.Fatalf("core row changed: %+v", row)
		}

		// The operator fixes the role; the retry path returns the request to
		// pending (RetrySelfDeletion's statement).
		f.now = f.now.Add(time.Hour)
		if _, err := pool.Exec(ctx, `
			UPDATE account_deletion_requests
			SET status='pending', attempt_count=0, next_attempt_at=$2,
				lease_until=NULL, lease_token=NULL, last_error_code='', updated_at=$2
			WHERE id=$1 AND status='manual_intervention'
		`, f.request.ID, f.now); err != nil {
			t.Fatal(err)
		}
		f.answer(user.DeletionStepEraseForms, answerCompleted(map[string]int64{"responses_redacted": 2}))
		if worked, err := f.run(worker); !worked || err != nil {
			t.Fatalf("worked=%v err=%v", worked, err)
		}
		if state := f.state(); state.Status != user.DeletionRequestCompleted {
			t.Fatalf("request = %+v", state)
		}
		if got := f.checkpoints(); !slices.Equal(got, sagaSteps) {
			t.Fatalf("checkpoints = %v", got)
		}
		for event, want := range map[string]int{
			"disable_identity": 1, "logout_sessions": 1, "erase_skymail": 1, "erase_cms": 1, "erase_forms": 2,
			"anonymize_core": 1, "delete_identity": 1,
		} {
			if got := f.events.count(event); got != want {
				t.Fatalf("%s ran %d times, want %d: %v", event, got, want, f.events.list())
			}
		}
		if forms := f.records()[user.DeletionStepEraseForms]; forms.Counts["responses_redacted"] != 2 {
			t.Fatalf("forms proof = %+v", forms)
		}
		f.assertNoPersonalData(f.state().LastErrorCode)
	})

	t.Run("resends the same command with fresh addresses after a crash", func(t *testing.T) {
		f := newSagaFixture(t, store)
		entered := make(chan struct{})
		var first sync.Once
		f.answer(user.DeletionStepEraseSkyMail, func(w http.ResponseWriter, r *http.Request) {
			crashed := false
			first.Do(func() {
				crashed = true
				close(entered)
				// The process dies during the call: the connection drops
				// before any answer.
				<-r.Context().Done()
			})
			if !crashed {
				answerCompleted(map[string]int64{"recipients_deleted": 1})(w, r)
			}
		})

		processCtx, kill := context.WithCancel(context.Background())
		died := make(chan error, 1)
		go func() {
			_, err := f.saga().RunOnce(processCtx)
			died <- err
		}()
		<-entered
		kill()
		if err := <-died; err == nil {
			t.Fatal("the killed pass reported success")
		} else {
			f.errors = append(f.errors, err.Error())
		}
		// Nothing recorded the failure: the claim stays until its lease ends.
		state := f.state()
		if state.Status != user.DeletionRequestProcessing || state.LeaseUntil == nil || state.AttemptCount != 1 {
			t.Fatalf("request after the crash = %+v", state)
		}
		if _, ok := f.records()[user.DeletionStepEraseSkyMail]; ok {
			t.Fatal("the unanswered call was checkpointed")
		}

		// Keycloak holds another address by the time the lease ends: the
		// new process reads the addresses again instead of reusing any.
		const added = "ada.legacy@example.org"
		f.identity.set([]string{sagaPersonal, sagaSchool, added}, nil)
		f.now = state.LeaseUntil.Add(time.Second)
		if worked, err := f.run(f.saga()); !worked || err != nil {
			t.Fatalf("restarted worker worked=%v err=%v", worked, err)
		}
		if state := f.state(); state.Status != user.DeletionRequestCompleted || state.AttemptCount != 2 {
			t.Fatalf("request = %+v", state)
		}
		// sentEmails checks that both calls carry the same request_id.
		if got := f.sentEmails(user.DeletionStepEraseSkyMail, 0); !slices.Equal(got, []string{sagaPersonal, sagaSchool}) {
			t.Fatalf("first command addresses = %v", got)
		}
		if got := f.sentEmails(user.DeletionStepEraseSkyMail, 1); !slices.Equal(got, []string{sagaPersonal, sagaSchool, added}) {
			t.Fatalf("resent command addresses = %v", got)
		}
		if f.events.count("read_addresses") != 2 || f.events.count("disable_identity") != 1 || f.events.count("logout_sessions") != 1 {
			t.Fatalf("events = %v", f.events.list())
		}
		if got := f.checkpoints(); !slices.Equal(got, sagaSteps) {
			t.Fatalf("checkpoints = %v", got)
		}
		f.assertNoPersonalData()
	})
}
