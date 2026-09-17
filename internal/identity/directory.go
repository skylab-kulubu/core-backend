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
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	FirstName   string    `json:"firstName"`
	LastName    string    `json:"lastName"`
	Username    string    `json:"username,omitempty"`
	SchoolEmail string    `json:"schoolEmail,omitempty"`
	SkyNumber   string    `json:"skyNumber,omitempty"`
}

type ClientRole struct {
	ClientID string `json:"clientId"`
	Role     string `json:"role"`
}

type UserCard struct {
	Person
	Groups         []Group      `json:"groups"`
	InheritedRoles []ClientRole `json:"inheritedRoles"`
	ExtraRoles     []ClientRole `json:"extraRoles"`
}

type Directory interface {
	ListGroups(ctx context.Context) ([]Group, error)
	GetGroup(ctx context.Context, idOrPath string) (Group, error)
	CreateGroup(ctx context.Context, parentRef, name string) (Group, error)
	UpdateGroup(ctx context.Context, g Group) (Group, error)
	Subgroups(ctx context.Context, groupID string) ([]Group, error)
	Members(ctx context.Context, groupID string) ([]Person, error)
	AddMember(ctx context.Context, groupID string, userID uuid.UUID) error
	RemoveMember(ctx context.Context, groupID string, userID uuid.UUID) error
	ListUsers(ctx context.Context) ([]Person, error)
	UsersWithClientRole(ctx context.Context, clientID, role string) ([]Person, error)
	CreateUser(ctx context.Context, p Person) (Person, error)
	GetUser(ctx context.Context, id uuid.UUID) (Person, error)
	DeleteUser(ctx context.Context, id uuid.UUID) error
	GroupsForUser(ctx context.Context, userID uuid.UUID) ([]Group, error)
	GroupClientRoles(ctx context.Context, groupID string) ([]ClientRole, error)
	SetGroupClientRoles(ctx context.Context, groupID string, roles []ClientRole) error
	UserExtraRoles(ctx context.Context, userID uuid.UUID) ([]ClientRole, error)
	AddUserExtraRole(ctx context.Context, userID uuid.UUID, role ClientRole) error
	RemoveUserExtraRole(ctx context.Context, userID uuid.UUID, role ClientRole) error
	LogoutAllSessions(ctx context.Context, userID uuid.UUID) error
	ReadSkyNumber(ctx context.Context, userID uuid.UUID) (string, error)
	WriteSkyNumber(ctx context.Context, userID uuid.UUID, skyNumber string) error
}
