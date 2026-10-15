package teammail

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// PersonReader reads a person from the directory (Keycloak).
type PersonReader interface {
	GetUser(ctx context.Context, id uuid.UUID) (identity.Person, error)
}

// Accounts reads core's account of a person.
type Accounts interface {
	Get(ctx context.Context, id uuid.UUID) (user.User, error)
	AttributionState(ctx context.Context, id uuid.UUID) (user.AttributionState, error)
}

// DirectoryPeople reads the people of a mail when it is sent: the address is
// Keycloak's primary e-mail, the name core's profile (Keycloak's for a person
// core has no row for), and core's account decides whether the person may
// still be written to.
type DirectoryPeople struct {
	Directory PersonReader
	Accounts  Accounts
}

// Recipient is the person a mail goes to. Nobody erased, being erased,
// disabled or gone gets one, nor an account without a primary e-mail.
func (p DirectoryPeople) Recipient(ctx context.Context, id uuid.UUID) (Recipient, SkipReason, error) {
	person, account, skip, err := p.read(ctx, id)
	if err != nil || skip != "" {
		return Recipient{}, skip, err
	}
	email := strings.TrimSpace(person.Email)
	if email == "" {
		return Recipient{}, SkipNoEmail, nil
	}
	return Recipient{Email: email, FullName: fullName(person, account)}, "", nil
}

// DisplayName is the full name of the person who made a change, or empty: a
// person core may no longer show, or one it cannot read now, has none in the
// mail (the template then leaves the line out).
func (p DirectoryPeople) DisplayName(ctx context.Context, id uuid.UUID) string {
	person, account, skip, err := p.read(ctx, id)
	if err != nil || skip != "" {
		return ""
	}
	return fullName(person, account)
}

// read is the person and core's row of them (nil without one), or why no
// mail may name them.
func (p DirectoryPeople) read(ctx context.Context, id uuid.UUID) (identity.Person, *user.User, SkipReason, error) {
	if id == user.DeletedSubject {
		return identity.Person{}, nil, SkipInactive, nil
	}
	var account *user.User
	stored, err := p.Accounts.Get(ctx, id)
	switch {
	case err == nil:
		if stored.AccountState != user.AccountActive {
			return identity.Person{}, nil, SkipInactive, nil
		}
		account = &stored
	case errors.Is(err, user.ErrNotFound):
		// No row: a deletion marker means the person was hard-purged.
		state, err := p.Accounts.AttributionState(ctx, id)
		if err != nil {
			return identity.Person{}, nil, "", err
		}
		if state == user.AttributionBlocked {
			return identity.Person{}, nil, SkipInactive, nil
		}
	default:
		return identity.Person{}, nil, "", err
	}
	person, err := p.Directory.GetUser(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return identity.Person{}, nil, SkipInactive, nil
	case err != nil:
		return identity.Person{}, nil, "", err
	case !person.Enabled:
		return identity.Person{}, nil, SkipInactive, nil
	}
	return person, account, "", nil
}

// fullName is core's profile name of the person, Keycloak's when core's row
// has none.
func fullName(person identity.Person, account *user.User) string {
	if account != nil {
		if name := joinName(account.FirstName, account.LastName); name != "" {
			return name
		}
	}
	return joinName(person.FirstName, person.LastName)
}

func joinName(first, last string) string {
	return strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
}
