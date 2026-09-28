// Package readlinksubject ensures the core row of a read link's subject,
// the person a product's read link is for (docs/media-lifecycle.md, read
// links). A read link names an active core account, and a Skyforms reviewer
// may never have signed in to core: their row is ensured from Keycloak, as
// the ticket and identity services ensure the row of a person they act on
// (dir.GetUser, then user.Service.Ensure).
package readlinksubject

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Subjects is media.ReadLinkSubjects over the identity directory and core's
// users.
type Subjects struct {
	dir   identity.Directory
	users user.Store
}

var _ media.ReadLinkSubjects = (*Subjects)(nil)

// New asks dir about a subject users has no row for, and ensures the row
// the way a first sign-in does, writing the Sky number core gives them back
// to dir.
func New(dir identity.Directory, users user.Store) *Subjects {
	return &Subjects{dir: dir, users: users}
}

func (s *Subjects) EnsureSubject(ctx context.Context, id uuid.UUID) error {
	if _, err := s.users.Get(ctx, id); err == nil {
		return nil
	} else if !errors.Is(err, user.ErrNotFound) {
		return err
	}
	person, err := s.dir.GetUser(ctx, id)
	if errors.Is(err, identity.ErrNotFound) {
		// Never there, or deleted (the erasure's delete_identity).
		return media.ErrLinkSubjectInactive
	}
	if err != nil {
		return lookupFailure(err)
	}
	// Disabled: by an admin, or by an erasure that has not deleted them yet.
	if !person.Enabled {
		return media.ErrLinkSubjectInactive
	}
	accounts := user.NewService(s.users, knownSkyNumber{dir: s.dir, skyNumber: person.SkyNumber})
	_, _, err = accounts.Ensure(ctx, id, user.Profile{
		Email:       person.Email,
		FirstName:   person.FirstName,
		LastName:    person.LastName,
		Username:    person.Username,
		SchoolEmail: person.SchoolEmail,
		SkyNumber:   person.SkyNumber,
	})
	switch {
	case errors.Is(err, user.ErrAccountBlocked):
		// Erased or being erased: its deletion marker keeps the row away.
		return media.ErrLinkSubjectInactive
	case errors.Is(err, user.ErrSkyNumberContended):
		return fmt.Errorf("%w: every Sky number tried was taken meanwhile", media.ErrLinkSubjectUnavailable)
	}
	return err
}

// lookupFailure sorts a failed lookup of the subject in the identity
// directory. Keycloak unreachable or answering 5xx or 429 may pass:
// ErrLinkSubjectUnavailable. Anything else (a permission core lacks, a wrong
// client secret, an answer that is not a user) is a misconfiguration:
// ErrLinkSubjectLookupFailed. Neither carries more than Keycloak's status:
// the address of a failed request names the person.
func lookupFailure(err error) error {
	if errors.Is(err, identity.ErrInvalid) {
		return fmt.Errorf("%w: Keycloak's answer is not a user", media.ErrLinkSubjectLookupFailed)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: Keycloak could not be reached", media.ErrLinkSubjectUnavailable)
	}
	status, ok := keycloakStatus(err)
	switch {
	case !ok:
		return fmt.Errorf("%w: the identity directory failed", media.ErrLinkSubjectLookupFailed)
	case status == 0:
		return fmt.Errorf("%w: Keycloak could not be reached", media.ErrLinkSubjectUnavailable)
	case status >= http.StatusInternalServerError || status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: Keycloak answered %d", media.ErrLinkSubjectUnavailable, status)
	default:
		return fmt.Errorf("%w: Keycloak answered %d", media.ErrLinkSubjectLookupFailed, status)
	}
}

// keycloakStatus is the status of Keycloak's answer to a gocloak request:
// 0 for a request that got no answer (refused, timed out). ok is false for
// an error that is not gocloak's.
func keycloakStatus(err error) (status int, ok bool) {
	var ptr *gocloak.APIError
	if errors.As(err, &ptr) && ptr != nil {
		return ptr.Code, true
	}
	var val gocloak.APIError
	if errors.As(err, &val) {
		return val.Code, true
	}
	return 0, false
}

// knownSkyNumber is Ensure's Sky number sync for a person just read from the
// directory: the read is answered from that person, so Keycloak is not read
// again, and the write goes to the directory.
type knownSkyNumber struct {
	dir       identity.Directory
	skyNumber string
}

func (k knownSkyNumber) ReadSkyNumber(context.Context, uuid.UUID) (string, error) {
	return k.skyNumber, nil
}

func (k knownSkyNumber) WriteSkyNumber(ctx context.Context, id uuid.UUID, skyNumber string) error {
	return k.dir.WriteSkyNumber(ctx, id, skyNumber)
}
