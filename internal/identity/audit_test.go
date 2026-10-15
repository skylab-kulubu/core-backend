package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Nerzal/gocloak/v13"
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

// A refusal Keycloak answered is a failed row; anything that may have come
// after Keycloak made the change is no refusal.
func TestRefusalTellsKnownFailuresFromUnknownOutcomes(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]struct {
		code    string
		refused bool
	}{
		ErrNotFound:                     {"not_found", true},
		fmt.Errorf("x: %w", ErrInvalid): {"invalid", true},
		ErrForbidden:                    {"forbidden", true},
		&gocloak.APIError{Code: 400, Message: "x"}: {"keycloak_400", true},
		gocloak.APIError{Code: 403}:                {"keycloak_403", true},
		&gocloak.APIError{Code: 502}:               {"keycloak_502", false},
		context.DeadlineExceeded:                   {"timeout", false},
		fmt.Errorf("put: %w", context.Canceled):    {"canceled", false},
		errors.New("dial tcp: connection reset"):   {"directory_error", false},
	} {
		code, refused := refusal(err)
		if code != want.code || refused != want.refused || !teamaudit.ValidErrorCode(code) {
			t.Errorf("%v: %q %v", err, code, refused)
		}
	}
}

// slowDirectory is a directory whose member writes fail with err after
// they may have reached Keycloak.
type slowDirectory struct {
	*Memory
	err error
}

func (d slowDirectory) AddMember(ctx context.Context, groupID string, userID uuid.UUID) error {
	_ = d.Memory.AddMember(ctx, groupID, userID)
	return d.err
}

// A timeout, a request given up or a lost connection leaves the row
// pending: Keycloak may have made the change, so the outcome is unknown.
// The caller gets the error.
func TestAuditedDirectoryLeavesAnUnknownOutcomePending(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, errors.New("connection reset"), &gocloak.APIError{Code: 503}} {
		dir, g, person := auditedMemory(t)
		memory := teamaudit.NewMemory()
		var lines []string
		audited := AuditedDirectory(slowDirectory{dir, cause}, memory, func(format string, args ...any) {
			lines = append(lines, fmt.Sprintf(format, args...))
		})
		if err := audited.AddMember(actorContext(), g.ID, person); !errors.Is(err, cause) && err != cause {
			t.Fatalf("%v: err %v", cause, err)
		}
		rows := memory.Rows()
		if len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomePending || rows[0].ErrorCode != "" {
			t.Fatalf("%v: rows %+v", cause, rows)
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "outcome unknown") || !strings.Contains(lines[0], rows[0].ID.String()) {
			t.Fatalf("%v: log %q", cause, lines)
		}
	}
	// A refusal finishes the row as failed.
	dir, g, person := auditedMemory(t)
	memory := teamaudit.NewMemory()
	audited := AuditedDirectory(slowDirectory{dir, &gocloak.APIError{Code: 400}}, memory, nil)
	_ = audited.AddMember(actorContext(), g.ID, person)
	if rows := memory.Rows(); len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomeFailed || rows[0].ErrorCode != "keycloak_400" {
		t.Fatalf("refused: %+v", rows)
	}
}
