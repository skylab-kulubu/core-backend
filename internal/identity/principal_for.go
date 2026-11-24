package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/user"
	"golang.org/x/sync/errgroup"
)

// ErrPrincipalUnavailable is a person's authority that could not be read
// from Keycloak or core. It never means the person has none, and never
// that they have some: the caller refuses (fail-closed).
var ErrPrincipalUnavailable = errors.New("identity: principal unavailable")

// PrincipalFor is the person userID as a token of theirs would stand for
// them now, read from Keycloak with core's service account instead of a
// token: their Groups (GroupsForUser, every one, so a person in more Groups
// than a token carries, the Group overage of ADR-0059, is read whole) and
// core's client roles they hold in effect (EffectiveClientRoles of the
// `core` client: directly, through their Groups and the Groups above them,
// the default roles and composites; the resource_access.core.roles a token
// carries). It is a person, never a service account or a product, and
// carries no Client (no token was issued).
//
// It is for a decision made when the person is not the caller: an
// Invitation accepted after its inviter's rights may have changed, an
// Access request approved, an Access review applied at the window's end.
// Its result goes to the same decisions as a token's (MembershipWriteDecision
// and the authz policy), which read the Leader groups fresh anyway.
//
// A person without an active account (unknown to core or Keycloak, erased
// or being erased, disabled in Keycloak) is ErrForbidden; a read that
// fails is ErrPrincipalUnavailable. Either way the Principal is the zero
// one, which no decision allows anything that needs a person.
func (s *service) PrincipalFor(ctx context.Context, userID uuid.UUID) (authz.Principal, error) {
	if userID == uuid.Nil || userID == user.DeletedSubject {
		return authz.Principal{}, ErrForbidden
	}
	active, err := s.activeAccount(ctx, userID)
	if err != nil {
		return authz.Principal{}, fmt.Errorf("%w: account", ErrPrincipalUnavailable)
	}
	if !active {
		return authz.Principal{}, ErrForbidden
	}
	var (
		person Person
		groups []Group
		roles  []string
	)
	reads, readCtx := errgroup.WithContext(ctx)
	reads.Go(func() (err error) {
		person, err = s.dir.GetUser(readCtx, userID)
		return principalRead(err, "person")
	})
	reads.Go(func() (err error) {
		groups, err = s.dir.GroupsForUser(readCtx, userID)
		return principalRead(err, "groups")
	})
	reads.Go(func() (err error) {
		roles, err = s.dir.EffectiveClientRoles(readCtx, userID, authn.ResourceAudience)
		return principalRead(err, "roles")
	})
	if err := reads.Wait(); err != nil {
		return authz.Principal{}, err
	}
	if !person.Enabled {
		return authz.Principal{}, ErrForbidden
	}
	p := authz.Principal{ID: userID.String(), Groups: make([]string, 0, len(groups)), Roles: roles}
	for _, g := range groups {
		if g.Path == "" {
			// A Group without its path cannot be decided on: refuse rather
			// than decide on part of the person's Groups.
			return authz.Principal{}, fmt.Errorf("%w: group path", ErrPrincipalUnavailable)
		}
		p.Groups = append(p.Groups, g.Path)
	}
	return p, nil
}

// principalRead is the error of one of PrincipalFor's reads: a person
// Keycloak does not know is ErrForbidden, any other failure
// ErrPrincipalUnavailable (never the cause's text, which can name the
// person).
func principalRead(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrClientNotFound):
		return fmt.Errorf("%w: %s", ErrPrincipalUnavailable, what)
	case errors.Is(err, ErrNotFound):
		return ErrForbidden
	}
	return fmt.Errorf("%w: %s", ErrPrincipalUnavailable, what)
}
