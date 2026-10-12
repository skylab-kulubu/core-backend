package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// finishFails is a writer whose outcomes cannot be written.
type finishFails struct{ *teamaudit.Memory }

func (finishFails) Finish(context.Context, uuid.UUID, teamaudit.Outcome, string, *teamaudit.State) error {
	return errors.New("teamaudit: finish: sqlstate_08006")
}

func auditedMemory(t *testing.T) (*Memory, Group, uuid.UUID) {
	t.Helper()
	dir := NewMemory()
	g := Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"}
	dir.PutGroup(g)
	person := uuid.New()
	dir.PutUser(Person{ID: person, Email: "ada@example.com"})
	return dir, g, person
}

func actorContext() context.Context {
	return teamaudit.WithActor(context.Background(), teamaudit.Actor{ID: uuid.New(), Basis: teamaudit.BasisPrivileged})
}

// An outcome that cannot be written leaves the row pending and is logged
// with the row's id and a code; the write's own result stands.
func TestAuditedDirectoryKeepsTheWriteWhenTheOutcomeIsLost(t *testing.T) {
	t.Parallel()
	dir, g, person := auditedMemory(t)
	memory := teamaudit.NewMemory()
	var lines []string
	audited := AuditedDirectory(dir, finishFails{memory}, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if err := audited.AddMember(actorContext(), g.ID, person); err != nil {
		t.Fatal(err)
	}
	rows := memory.Rows()
	if len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomePending {
		t.Fatalf("rows %+v", rows)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], rows[0].ID.String()) || !strings.Contains(lines[0], "sqlstate_08006") ||
		strings.Contains(lines[0], "ada@example.com") {
		t.Fatalf("log %q", lines)
	}
}

// A write without an actor in its context is refused before anything is
// asked of the directory or written.
func TestAuditedDirectoryRefusesAWriteWithoutAnActor(t *testing.T) {
	t.Parallel()
	dir, g, person := auditedMemory(t)
	memory := teamaudit.NewMemory()
	audited := AuditedDirectory(dir, memory, nil)
	dir.Ops = nil
	if err := audited.AddMember(context.Background(), g.ID, person); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	for _, op := range dir.Ops {
		if op == "AddMember" {
			t.Fatal("the write reached the directory")
		}
	}
	if len(memory.Rows()) != 0 {
		t.Fatal("a row without an actor")
	}
}

// A read that fails before the row is opened fails the write, unrecorded:
// nothing was asked of Keycloak's write side.
func TestAuditedDirectoryFailsOnAnUnknownGroup(t *testing.T) {
	t.Parallel()
	dir, _, person := auditedMemory(t)
	memory := teamaudit.NewMemory()
	audited := AuditedDirectory(dir, memory, nil)
	if err := audited.RemoveMember(actorContext(), uuid.NewString(), person); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if len(memory.Rows()) != 0 {
		t.Fatal("a row for a Group that is not there")
	}
}

func TestDirectoryErrorCodes(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]string{
		ErrNotFound:                         "not_found",
		fmt.Errorf("x: %w", ErrInvalid):     "invalid",
		ErrForbidden:                        "forbidden",
		context.DeadlineExceeded:            "timeout",
		errors.New("keycloak 500 ada@x.io"): "directory_error",
	} {
		if got := directoryErrorCode(err); got != want || !teamaudit.ValidErrorCode(got) {
			t.Errorf("%v: %q", err, got)
		}
	}
}
