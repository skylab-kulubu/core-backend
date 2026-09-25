package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// IdentityAddresses is the identity provider's read-only view of the
// person's addresses: the Primary e-mail and the School and Personal e-mail
// attributes. A person the provider no longer knows has none (nil, nil).
type IdentityAddresses interface {
	UserAddresses(context.Context, uuid.UUID) ([]string, error)
}

// CoreUsers reads core's own row of the person.
type CoreUsers interface {
	Get(context.Context, uuid.UUID) (user.User, error)
}

type erasureAddresses struct {
	identity IdentityAddresses
	users    CoreUsers
}

// NewErasureAddresses is the address source of the erasure saga (ADR-0051):
// what Keycloak holds (`email`, `schoolEmail`, `personalEmail`) together with
// core's `users.email` and `users.school_email`, trimmed, lower-cased and
// without blanks or repeats. It is read afresh for every pass and nothing
// keeps it.
//
// Keycloak unreachable is an error: going on with core's row alone would
// erase less than the person holds. Keycloak no longer knowing the person
// leaves core's row. More than three addresses is refused rather than cut,
// since any address left out would keep its data.
func NewErasureAddresses(identity IdentityAddresses, users CoreUsers) ErasureAddressSource {
	return erasureAddresses{identity: identity, users: users}
}

func (a erasureAddresses) ErasureAddresses(ctx context.Context, subjectID uuid.UUID) ([]string, error) {
	if a.identity == nil || a.users == nil {
		return nil, errors.New("erasure addresses: source not configured")
	}
	addresses, err := a.identity.UserAddresses(ctx, subjectID)
	if err != nil {
		// The identity adapter's errors name neither the subject nor an
		// address; they tell the operator what failed.
		return nil, fmt.Errorf("erasure addresses: %w", err)
	}
	row, err := a.users.Get(ctx, subjectID)
	switch {
	case errors.Is(err, user.ErrNotFound):
	case err != nil:
		return nil, errors.New("erasure addresses: core user row could not be read")
	default:
		addresses = append(addresses, row.Email, row.SchoolEmail)
	}
	emails, err := erasure.NormalizeEmails(addresses)
	if err != nil {
		return nil, fmt.Errorf("erasure addresses: %w", err)
	}
	return emails, nil
}
