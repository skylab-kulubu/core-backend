package identity

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var (
	ErrNotFound  = errors.New("identity: not found")
	ErrForbidden = errors.New("identity: forbidden")
	ErrInvalid   = errors.New("identity: invalid")
)

type Group struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Path       string            `json:"path"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type Person struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	FirstName string    `json:"firstName"`
	LastName  string    `json:"lastName"`
	Username  string    `json:"username,omitempty"`
}

type Directory interface {
	ListGroups(ctx context.Context) ([]Group, error)
	GetGroup(ctx context.Context, idOrPath string) (Group, error)
	Subgroups(ctx context.Context, groupID string) ([]Group, error)
	Members(ctx context.Context, groupID string) ([]Person, error)
	AddMember(ctx context.Context, groupID string, userID uuid.UUID) error
	RemoveMember(ctx context.Context, groupID string, userID uuid.UUID) error
	CreateUser(ctx context.Context, p Person) (Person, error)
	GetUser(ctx context.Context, id uuid.UUID) (Person, error)
	DeleteUser(ctx context.Context, id uuid.UUID) error
}
