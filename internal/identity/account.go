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

func ignoreMissing(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
