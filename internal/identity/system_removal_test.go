package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type systemRemover interface {
	RemoveMemberAsSystem(ctx context.Context, client, groupRef string, userID uuid.UUID) error
}

// The management term job's removal (ADR-0078 karar 3, 2026-11-24): no
// person decides, so nothing is authorized; the row is the system's, with
// the job as its client; the notifiers hear of it with no actor; only a
// direct membership is removed.
func TestRemoveMemberAsSystem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := identity.NewMemory()
	weblab, leaders := uuid.NewString(), uuid.NewString()
	for _, g := range []identity.Group{
		{ID: uuid.NewString(), Name: "UYELER", Path: "/UYELER"},
		{ID: uuid.NewString(), Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: weblab, Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: leaders, Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
	} {
		dir.PutGroup(g)
	}
	leader, other := uuid.New(), uuid.New()
	dir.PutUser(identity.Person{ID: leader, Email: "ada@example.test", FirstName: "Ada"})
	dir.PutUser(identity.Person{ID: other, Email: "can@example.test", FirstName: "Can"})
	if err := dir.AddMember(ctx, leaders, leader); err != nil {
		t.Fatal(err)
	}
	audit := teamaudit.NewMemory()
	spy := &membershipSpy{}
	svc := identity.NewServiceWithOptions(identity.AuditedDirectory(dir, audit, nil), user.NewMemoryStore(),
		authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{Membership: spy})
	remover, ok := svc.(systemRemover)
	if !ok {
		t.Fatal("the identity service has no system removal")
	}

	// Not directly in WEBLAB (only in its Leader group), and not in it at
	// all: nothing is written.
	for _, c := range []struct {
		group  string
		person uuid.UUID
	}{{weblab, leader}, {leaders, other}} {
		if err := remover.RemoveMemberAsSystem(ctx, teamaudit.SystemTermRemoval, c.group, c.person); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("%s %s: %v", c.group, c.person, err)
		}
	}
	if rows := audit.Rows(); len(rows) != 0 || len(spy.changes) != 0 {
		t.Fatalf("rows %+v changes %+v", rows, spy.changes)
	}

	if err := remover.RemoveMemberAsSystem(ctx, teamaudit.SystemTermRemoval, leaders, leader); err != nil {
		t.Fatal(err)
	}
	members, err := dir.Members(ctx, leaders)
	if err != nil || len(members) != 0 {
		t.Fatalf("members %v %v", members, err)
	}
	rows := audit.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	row := rows[0]
	if row.Action != teamaudit.ActionMemberRemove || row.Actor.Basis != teamaudit.BasisSystem || row.Actor.ID != uuid.Nil ||
		row.Actor.Client != teamaudit.SystemTermRemoval || row.TargetUserID != leader || row.GroupID != leaders || row.Outcome != teamaudit.OutcomeOK {
		t.Fatalf("row %+v", row)
	}
	if len(spy.changes) != 1 || spy.changes[0].Added || spy.changes[0].UserID != leader || spy.changes[0].ActorID != "" ||
		spy.changes[0].Group.Path != "/UYELER/ARGE/WEBLAB/LIDERLER" {
		t.Fatalf("changes %+v", spy.changes)
	}

	// Another job's name is refused by the record: no row, no change.
	if err := dir.AddMember(ctx, leaders, other); err != nil {
		t.Fatal(err)
	}
	if err := remover.RemoveMemberAsSystem(ctx, teamaudit.SystemTermExpiry, leaders, other); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("the expiry's name: %v", err)
	}
	if members, _ := dir.Members(ctx, leaders); len(members) != 1 {
		t.Fatalf("removed without a record: %v", members)
	}
}
