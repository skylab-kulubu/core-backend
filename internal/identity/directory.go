package identity

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var (
	ErrNotFound                 = errors.New("identity: not found")
	ErrForbidden                = errors.New("identity: forbidden")
	ErrInvalid                  = errors.New("identity: invalid")
	ErrAccountErasureDisabled   = errors.New("identity: account erasure disabled")
	ErrAccountAccessUnavailable = errors.New("identity: account access projection unavailable")
)

type Group struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Path       string            `json:"path"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// GroupPaths lists the full paths of groups, in their order.
func GroupPaths(groups []Group) []string {
	paths := make([]string, 0, len(groups))
	for _, group := range groups {
		paths = append(paths, group.Path)
	}
	return paths
}

type Person struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	FirstName   string    `json:"firstName"`
	LastName    string    `json:"lastName"`
	Username    string    `json:"username,omitempty"`
	SchoolEmail string    `json:"schoolEmail,omitempty"`
	SkyNumber   string    `json:"skyNumber,omitempty"`
	// Enabled is Keycloak's enabled flag. The zero value is disabled: only
	// the directory's reads set it (Keycloak's user and user list; the
	// memory directory reports a person enabled until its DisableUser).
	// Read-link issuance uses it to decide whether to ensure a core row for
	// a person core has none for, and the group count report to leave out
	// users who get no token. Nothing else branches on it, and it is never
	// part of an answer.
	Enabled bool `json:"-"`
}

type GroupMember struct {
	Person
	SourceGroupID   string `json:"sourceGroupId,omitempty"`
	SourceGroupPath string `json:"sourceGroupPath,omitempty"`
}

type ClientRole struct {
	ClientID string `json:"clientId"`
	Role     string `json:"role"`
}

type UserCard struct {
	Person
	Linkedin   string `json:"linkedin,omitempty"`
	University string `json:"university,omitempty"`
	Faculty    string `json:"faculty,omitempty"`
	Department string `json:"department,omitempty"`
	// YTULinked marks university, faculty and department as following the
	// YTÜ login; an admin edit that changes them is refused.
	YTULinked         bool         `json:"ytuLinked,omitempty"`
	Phone             string       `json:"phone,omitempty"`
	StudentCardUid    string       `json:"studentCardUid,omitempty"`
	ProfilePictureURL string       `json:"profilePictureUrl,omitempty"`
	Groups            []Group      `json:"groups,omitempty"`
	InheritedRoles    []ClientRole `json:"inheritedRoles,omitempty"`
	ExtraRoles        []ClientRole `json:"extraRoles,omitempty"`
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
	SearchUsers(ctx context.Context, query string, limit int) ([]Person, error)
	ListClientRoles(ctx context.Context) ([]ClientRole, error)
	UsersWithClientRole(ctx context.Context, clientID, role string) ([]Person, error)
	CreateUser(ctx context.Context, p Person) (Person, error)
	GetUser(ctx context.Context, id uuid.UUID) (Person, error)
	DisableUser(ctx context.Context, id uuid.UUID) error
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
