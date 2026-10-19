package commpref_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The tracking switch on the table: one row, an audit row per change of
// value, a withdrawal per "off" (a newer one replaces a waiting one and
// clears its claim), the claim fence, and erasure of all three.
func TestPostgresTrackingStore(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := commpref.NewPostgresStore(pool)
	person := uuid.New()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	write := func(on bool, at time.Time) {
		t.Helper()
		if err := store.Apply(ctx, person, []commpref.Change{{Channel: commpref.ChannelEmail, Category: commpref.Tracking(), Enabled: &on}}, commpref.SourceAccountCenter, at); err != nil {
			t.Fatal(err)
		}
	}
	write(false, now)
	write(false, now.Add(time.Second))
	rows, err := store.List(ctx, person)
	if err != nil || len(rows) != 1 || rows[0].Category != commpref.Tracking() || rows[0].Enabled || rows[0].Channel != commpref.ChannelEmail {
		t.Fatalf("rows %+v %v", rows, err)
	}
	claimed, err := store.ClaimWithdrawals(ctx, now.Add(time.Minute), 2*time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].Subject != person || !claimed[0].RequestedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	if again, err := store.ClaimWithdrawals(ctx, now.Add(time.Minute), 2*time.Minute, 10); err != nil || len(again) != 0 {
		t.Fatalf("claimed twice %+v %v", again, err)
	}
	// A newer "off" while the send is in flight: the late answer of the
	// older claim neither deletes nor delays it.
	write(true, now.Add(2*time.Minute))
	write(false, now.Add(3*time.Minute))
	if err := store.CompleteWithdrawal(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryWithdrawal(ctx, claimed[0], now.Add(time.Hour), 503); err != nil {
		t.Fatal(err)
	}
	if n, age, err := store.WithdrawalBacklog(ctx, now.Add(4*time.Minute)); err != nil || n != 1 || age != time.Minute {
		t.Fatalf("backlog %d %s %v", n, age, err)
	}
	claimed, err = store.ClaimWithdrawals(ctx, now.Add(3*time.Minute), 2*time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 0 {
		t.Fatalf("claim the newer %+v %v", claimed, err)
	}
	if err := store.RetryWithdrawal(ctx, claimed[0], now.Add(time.Hour), 503); err != nil {
		t.Fatal(err)
	}
	var attempts, status int
	if err := pool.QueryRow(ctx, `SELECT attempts, last_status FROM communication_tracking_withdrawals WHERE user_id = $1`, person).Scan(&attempts, &status); err != nil || attempts != 1 || status != 503 {
		t.Fatalf("retry %d %d %v", attempts, status, err)
	}
	if early, _ := store.ClaimWithdrawals(ctx, now.Add(59*time.Minute), time.Minute, 10); len(early) != 0 {
		t.Fatalf("claimed before due %+v", early)
	}
	claimed, _ = store.ClaimWithdrawals(ctx, now.Add(time.Hour), time.Minute, 10)
	if len(claimed) != 1 || claimed[0].Attempts != 1 {
		t.Fatalf("due again %+v", claimed)
	}
	if err := store.CompleteWithdrawal(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	if n, _, err := store.WithdrawalBacklog(ctx, now); err != nil || n != 0 {
		t.Fatalf("after complete %d %v", n, err)
	}

	audit, err := store.TrackingChanges(ctx, person)
	if err != nil || len(audit) != 3 || audit[0].Enabled || audit[0].Previous != nil || !audit[1].Enabled || audit[1].Previous == nil || *audit[1].Previous ||
		audit[2].Enabled || audit[2].Previous == nil || !*audit[2].Previous || audit[0].Source != commpref.SourceAccountCenter || !audit[0].ChangedAt.Equal(now) {
		t.Fatalf("audit %+v %v", audit, err)
	}

	// Another person's News rows and tracking row stay through this one's
	// erasure; this one's go from all three tables.
	other := uuid.New()
	off := false
	if err := store.Apply(ctx, other, []commpref.Change{{Channel: commpref.ChannelEmail, Category: commpref.Tracking(), Enabled: &off}}, commpref.SourceAccountCenter, now); err != nil {
		t.Fatal(err)
	}
	write(false, now.Add(5*time.Minute))
	if err := store.Apply(ctx, person, []commpref.Change{{Channel: commpref.ChannelEmail, Category: commpref.Master(), Enabled: &off}}, commpref.SourceAccountCenter, now); err != nil {
		t.Fatal(err)
	}
	if n, err := store.EraseSubject(ctx, person); err != nil || n != 2 {
		t.Fatalf("erase %d %v", n, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM communication_preferences WHERE user_id = $1) +
		(SELECT count(*) FROM communication_tracking_withdrawals WHERE user_id = $1) +
		(SELECT count(*) FROM communication_tracking_changes WHERE user_id = $1)`, person).Scan(&left); err != nil || left != 0 {
		t.Fatalf("left %d %v", left, err)
	}
	if n, _, _ := store.WithdrawalBacklog(ctx, now); n != 1 {
		t.Fatalf("the other person's withdrawal went: %d", n)
	}

	// A person being erased gets no row, no audit row and no withdrawal.
	users := user.NewPostgresStore(pool)
	holder := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, holder, user.Profile{Email: "tracking-holder@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.RequestDeletion(ctx, holder, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(ctx, holder, []commpref.Change{{Channel: commpref.ChannelEmail, Category: commpref.Tracking(), Enabled: &off}}, commpref.SourceAccountCenter, now); !errors.Is(err, commpref.ErrSubjectInactive) {
		t.Fatalf("inactive: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM communication_tracking_withdrawals WHERE user_id = $1) +
		(SELECT count(*) FROM communication_tracking_changes WHERE user_id = $1)`, holder).Scan(&left); err != nil || left != 0 {
		t.Fatalf("inactive left %d %v", left, err)
	}
}
