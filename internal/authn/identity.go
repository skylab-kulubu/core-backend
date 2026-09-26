package authn

import (
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const LocalsIdentity = "identity"
const LocalsUser = "user"

type Identity struct {
	ID      uuid.UUID
	Profile user.Profile
	Groups  []string
	Roles   []string
	// Client is the Keycloak client the token was issued to (`azp`).
	Client string
	// ServiceAccount is true for a client-credentials token: the Client's
	// own service account, never a person. Keycloak writes the `client_id`
	// claim (the service_account client scope, a user session note) only
	// into a service account's tokens.
	ServiceAccount bool
}
