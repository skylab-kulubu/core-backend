// Package readlinksubject ensures the core row of the person a product's
// read link is for (docs/media-lifecycle.md, read links). A read link names
// an active core account, and a Skyforms reviewer may never have signed in
// to core: their row is ensured from Keycloak, as the ticket and identity
// services ensure the row of a person they act on (dir.GetUser, then
// user.Service.Ensure).
package readlinksubject

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Accounts is media.ReadLinkSubjects over the identity directory and core's
// users.
type Accounts struct {
	dir      identity.Directory
	users    user.Store
	accounts user.Service
}

var _ media.ReadLinkSubjects = (*Accounts)(nil)

// New asks dir about a person users has no row for, and ensures the row the
// way a sign-in does (the Sky number included).
func New(dir identity.Directory, users user.Store) *Accounts {
	return &Accounts{dir: dir, users: users, accounts: user.NewService(users, dir)}
}

func (a *Accounts) EnsureAccount(ctx context.Context, id uuid.UUID) error {
	if _, err := a.users.Get(ctx, id); err == nil {
		return nil
	} else if !errors.Is(err, user.ErrNotFound) {
		return err
	}
	person, err := a.dir.GetUser(ctx, id)
	if errors.Is(err, identity.ErrNotFound) {
		// Never there, or deleted (the erasure's delete_identity).
		return media.ErrLinkSubjectInactive
	}
	if err != nil {
		// Unreachable, or an answer that is not the person: core cannot tell
		// whether they may have a row.
		return fmt.Errorf("%w: %w", media.ErrLinkSubjectUnavailable, err)
	}
	// Disabled: by an admin, or by an erasure that has not deleted them yet.
	if !person.Enabled {
		return media.ErrLinkSubjectInactive
	}
	_, _, err = a.accounts.Ensure(ctx, id, user.Profile{
		Email:       person.Email,
		FirstName:   person.FirstName,
		LastName:    person.LastName,
		Username:    person.Username,
		SchoolEmail: person.SchoolEmail,
		SkyNumber:   person.SkyNumber,
	})
	if errors.Is(err, user.ErrAccountBlocked) {
		// Erased or being erased: its deletion marker keeps the row away.
		return media.ErrLinkSubjectInactive
	}
	return err
}
