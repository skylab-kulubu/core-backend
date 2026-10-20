package event_test

import (
	"context"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The database derives registrationOpen as the Go code does, writes the two
// closure columns on create and update, and an update that leaves them out
// (a whole-Event write from Update) writes what it is given.
func TestPostgresStoreDerivesRegistrationOpen(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	now := time.Now()
	for _, tc := range registrationCases() {
		in := tc.event(now)
		created, err := store.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE events SET status = $2 WHERE id = $1`, created.ID, string(tc.status)); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RegistrationClosed != tc.closed || (got.RegistrationClosesAt == nil) != (tc.deadline == "") {
			t.Errorf("%s: stored closed=%v closesAt=%v", tc.name, got.RegistrationClosed, got.RegistrationClosesAt)
		}
		if got.RegistrationOpen != tc.open || got.RegistrationOpenAt(time.Now()) != tc.open {
			t.Errorf("%s: registrationOpen = %v, want %v", tc.name, got.RegistrationOpen, tc.open)
		}
	}

	open, err := store.Create(ctx, event.Event{Name: "Açık", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if !open.RegistrationOpen || open.RegistrationClosed || open.RegistrationClosesAt != nil {
		t.Fatalf("created %+v", open)
	}
	deadline := now.Add(time.Hour).UTC().Truncate(time.Microsecond)
	open.RegistrationClosed, open.RegistrationClosesAt = true, &deadline
	updated, err := store.Update(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.RegistrationClosed || updated.RegistrationClosesAt == nil || !updated.RegistrationClosesAt.Equal(deadline) || updated.RegistrationOpen {
		t.Fatalf("updated %+v", updated)
	}
}
