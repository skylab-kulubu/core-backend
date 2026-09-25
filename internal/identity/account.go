package identity

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

type AccountIdentity struct {
	directory Directory
}

func NewAccountIdentity(directory Directory) *AccountIdentity {
	return &AccountIdentity{directory: directory}
}

func (a *AccountIdentity) EnsureDisabled(ctx context.Context, id uuid.UUID) error {
	return ignoreMissing(a.directory.DisableUser(ctx, id))
}

func (a *AccountIdentity) EnsureLoggedOut(ctx context.Context, id uuid.UUID) error {
	return ignoreMissing(a.directory.LogoutAllSessions(ctx, id))
}

func (a *AccountIdentity) EnsureDeleted(ctx context.Context, id uuid.UUID) error {
	return ignoreMissing(a.directory.DeleteUser(ctx, id))
}

// userAddressReader is the identity provider's read-only view of a person's
// addresses. Only the Keycloak directory has one.
type userAddressReader interface {
	UserAddresses(context.Context, uuid.UUID) ([]string, error)
}

// UserAddresses returns the addresses the identity provider holds for the
// person, for account erasure. A person the provider no longer knows has
// none; the saga then goes on with core's own row. A directory that cannot
// read them (the in-memory development one) refuses, because an empty answer
// would erase less than the person holds.
func (a *AccountIdentity) UserAddresses(ctx context.Context, id uuid.UUID) ([]string, error) {
	reader, ok := a.directory.(userAddressReader)
	if !ok {
		return nil, errors.New("identity: directory cannot read user addresses")
	}
	addresses, err := reader.UserAddresses(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return addresses, err
}

func ignoreMissing(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
