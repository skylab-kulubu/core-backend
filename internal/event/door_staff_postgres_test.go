package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestCreateRollsBackEventWhenDoorStaffGuardRejects(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	eventID := uuid.New()
	_, err := store.Create(ctx, event.Event{
		ID: eventID, Name: "Atomic create", Location: "YTÜ", OwnerTeam: "WEBLAB",
		DoorStaffIDs: []uuid.UUID{uuid.New()},
	})
	if !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("create error = %v, want forbidden", err)
	}
	var events, staff int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE id=$1`, eventID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_door_staff WHERE event_id=$1`, eventID).Scan(&staff); err != nil {
		t.Fatal(err)
	}
	if events != 0 || staff != 0 {
		t.Fatalf("rejected create persisted event=%d staff=%d", events, staff)
	}
}

func TestUpdateRollsBackEventAndDoorStaffReplacementWhenGuardRejects(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	staffID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, staffID, user.Profile{Email: "door-staff@example.test"}); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	created, err := store.Create(ctx, event.Event{
		Name: "Original", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staffID},
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := created
	changed.Name = "Should roll back"
	changed.DoorStaffIDs = []uuid.UUID{uuid.New()}
	if _, err := store.Update(ctx, changed); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("update error = %v, want forbidden", err)
	}
	after, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "Original" || len(after.DoorStaffIDs) != 1 || after.DoorStaffIDs[0] != staffID {
		t.Fatalf("rejected update partially committed: %+v", after)
	}
}
