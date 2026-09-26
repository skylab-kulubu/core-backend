package authn

import (
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
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
	// Product is the product whose service account the caller is, resolved
	// from Client through the configured ServiceClients
	// (WithServiceProducts). Empty for a person.
	Product authz.Product
}

// WithServiceProducts resolves, for every token parse accepts, the product a
// service account's token speaks for.
func WithServiceProducts(parse func(string) (Identity, error), clients authz.ServiceClients) func(string) (Identity, error) {
	return func(token string) (Identity, error) {
		ident, err := parse(token)
		if err != nil {
			return ident, err
		}
		ident.Product = clients.Product(ident.Client, ident.ServiceAccount)
		return ident, nil
	}
}
