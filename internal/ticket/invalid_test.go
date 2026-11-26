package ticket_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

func invalidCode(t *testing.T, err error) string {
	t.Helper()
	var refused *ticket.InvalidError
	if !errors.As(err, &refused) || !errors.Is(err, ticket.ErrInvalid) {
		t.Fatalf("not an InvalidError that is ErrInvalid: %v", err)
	}
	return refused.Code
}

// A token whose subject is not a person's id is refused with a code, as is
// an Idempotency-Key past its length.
func TestInvalidErrorCodes(t *testing.T) {
	t.Parallel()
	if _, err := ticket.RequestHash(strings.Repeat("k", 129)); invalidCode(t, err) != ticket.CodeRequestKeyTooLong {
		t.Fatalf("request key: %v", err)
	}
	if hash, err := ticket.RequestHash(strings.Repeat("k", 128)); err != nil || hash == "" {
		t.Fatalf("a key of 128: %q %v", hash, err)
	}
	svc := ticket.NewService(ticket.NewMemoryStore(), event.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	_, err := svc.CheckInMe(t.Context(), authz.Principal{ID: "service-account"}, uuid.New(), "")
	if invalidCode(t, err) != ticket.CodeTokenSubjectInvalid {
		t.Fatalf("subject: %v", err)
	}
}
