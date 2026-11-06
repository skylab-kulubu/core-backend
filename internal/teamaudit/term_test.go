package teamaudit

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// The management term rows (docs/management-membership-terms.md): the
// daily job's expiry row is the system's, with the nil subject and the
// job's name as its client; no other action takes that basis, and a person
// is always the target.
func TestTermRowsAndTheSystemBasis(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemory()
	group, person := uuid.NewString(), uuid.New()
	system := Actor{Basis: BasisSystem, Client: SystemTermExpiry}
	expired := Record{Actor: system, Action: ActionTermExpired, GroupID: group, GroupPath: "/UYELER/YK", TargetUserID: person,
		After: &State{EndsAt: "2027-05-31T20:59:59Z"}}
	id, err := m.Begin(ctx, expired)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Finish(ctx, id, OutcomeOK, "", nil); err != nil {
		t.Fatal(err)
	}
	rows := m.Rows()
	if len(rows) != 1 || rows[0].Actor.ID != uuid.Nil || rows[0].Actor.Client != "system:term-expiry" || rows[0].After.EndsAt != "2027-05-31T20:59:59Z" {
		t.Fatalf("rows %+v", rows)
	}
	for name, r := range map[string]Record{
		"system extends":        {Actor: system, Action: ActionTermExtend, GroupID: group, GroupPath: "/UYELER/YK", TargetUserID: person},
		"system with a subject": {Actor: Actor{ID: uuid.New(), Basis: BasisSystem}, Action: ActionTermExpired, GroupID: group, GroupPath: "/UYELER/YK", TargetUserID: person},
		"no target":             {Actor: system, Action: ActionTermExpired, GroupID: group, GroupPath: "/UYELER/YK"},
		"a person's nil actor":  {Actor: Actor{Basis: BasisPrivileged}, Action: ActionTermExtend, GroupID: group, GroupPath: "/UYELER/YK", TargetUserID: person},
	} {
		if _, err := m.Begin(ctx, r); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	successor := uuid.New()
	handover := Record{Actor: Actor{ID: uuid.New(), Basis: BasisPrivileged}, Action: ActionTermHandover, GroupID: group, GroupPath: "/UYELER/YK",
		TargetUserID: person, After: &State{SuccessorID: successor.String(), Handover: "immediate", EndsAt: "x"}}
	if _, err := m.Begin(ctx, handover); err != nil {
		t.Fatal(err)
	}
	// A successor that is not a subject is not kept.
	handover.After = &State{SuccessorID: "ada@example.test"}
	if _, err := m.Begin(ctx, handover); err != nil {
		t.Fatal(err)
	}
	rows = m.Rows()
	if rows[1].After.SuccessorID != successor.String() || rows[2].After.SuccessorID != "" {
		t.Fatalf("successors %q %q", rows[1].After.SuccessorID, rows[2].After.SuccessorID)
	}
}
