package subjectrequest_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/optional"
	"github.com/skylab-kulubu/core-backend/internal/subjectrequest"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// The service on the table: the register computes the due date, a change
// and its trail row land together, the list's order is the memory store's,
// an unknown account is refused and a known one's request outlives it.
func TestPostgresStore(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	clock := now
	svc := subjectrequest.NewService(subjectrequest.NewPostgresStore(pool), authz.NewAuthorizer(authz.DefaultPolicy()),
		func() time.Time { return clock })

	account := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email) VALUES ($1, 'ada@example.test')`, account); err != nil {
		t.Fatal(err)
	}
	in := valid()
	accountID := account.String()
	in.UserID = &accountID
	created, err := svc.Create(ctx, yk, in)
	if err != nil {
		t.Fatal(err)
	}
	if created.DueOn != "2026-11-19" || created.ReceivedOn != "2026-10-20" || created.UserID == nil || *created.UserID != account ||
		!slices.Equal(created.RequestTypes, []subjectrequest.Type{subjectrequest.TypeLearn, subjectrequest.TypeErase}) ||
		created.CreatedBy == nil || created.CreatedBy.String() != yk.ID || created.Responder != subjectrequest.DefaultResponder {
		t.Fatalf("created %+v", created)
	}

	unknown := uuid.NewString()
	in.UserID = &unknown
	if _, err := svc.Create(ctx, yk, in); code(err) != "user_unknown" {
		t.Fatalf("unknown account: %v", err)
	}

	clock = now.Add(time.Minute)
	answered := "2026-10-26"
	closed, err := svc.Update(ctx, admin, created.ID, subjectrequest.Patch{
		Status:     optional.Field[subjectrequest.Status]{Set: true, Value: subjectrequest.StatusRejected},
		AnsweredOn: optional.Field[*string]{Set: true, Value: &answered},
		UserID:     optional.Field[*string]{Set: true, Null: true},
	})
	if err != nil || closed.AnsweredOn == nil || *closed.AnsweredOn != answered || closed.UserID != nil ||
		closed.UpdatedBy.String() != admin.ID || !closed.UpdatedAt.Equal(clock) || closed.CreatedBy.String() != yk.ID {
		t.Fatalf("closed %+v %v", closed, err)
	}
	read, err := svc.Get(ctx, yk, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Events) != 2 || read.Events[1].ActorID.String() != admin.ID ||
		!slices.Equal(read.Events[1].Fields, []string{"userId", "status", "answeredOn"}) || read.Events[1].Status != subjectrequest.StatusRejected {
		t.Fatalf("trail %+v", read.Events)
	}

	open := valid()
	open.ReceivedOn = "2026-10-01"
	early, err := svc.Create(ctx, yk, open)
	if err != nil {
		t.Fatal(err)
	}
	open.ReceivedOn = "2026-10-25"
	late, err := svc.Create(ctx, yk, open)
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx, yk)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for _, r := range list {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []uuid.UUID{early.ID, late.ID, created.ID}) {
		t.Fatalf("order %v", ids)
	}

	// The account goes, its request stays.
	in.UserID = &accountID
	kept, err := svc.Create(ctx, yk, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, account); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Get(ctx, yk, kept.ID); err != nil || got.UserID != nil || got.RequesterName != "Ada Lovelace" {
		t.Fatalf("after the account %+v %v", got, err)
	}
}
