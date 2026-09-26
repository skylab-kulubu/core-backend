package authz

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Product is a SKY LAB product that owns records: core itself, or another
// product whose service account calls core.
type Product string

const (
	// ProductCore is core itself. No caller is core.
	ProductCore Product = "core"
	// ProductForms is Skyforms (forms-backend).
	ProductForms Product = "forms"
	// ProductCMS is the CMS (cms-backend).
	ProductCMS Product = "cms"
)

// ServiceProducts are the products that call core with a service account of
// their own.
var ServiceProducts = []Product{ProductForms, ProductCMS}

// ServiceClients maps the Keycloak client of each product's service account
// to the product. A product with no client here has no service identity core
// accepts.
type ServiceClients map[string]Product

// ServiceClientsEnv configures ServiceClients: product:client pairs,
// separated by commas. "none" configures no product.
const ServiceClientsEnv = "MEDIA_SERVICE_CLIENTS"

// DefaultServiceClients is used when ServiceClientsEnv is unset: forms-backend
// requests its service token as the forms client (KEYCLOAK_CLIENT_ID). The CMS
// has no service account yet.
const DefaultServiceClients = "forms:forms"

// ErrServiceClientsInvalid is a ServiceClientsEnv value core cannot read. Core
// refuses to start with one.
var ErrServiceClientsInvalid = errors.New(ServiceClientsEnv + ": invalid")

var clientID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// ServiceClientsFromEnv reads ServiceClientsEnv.
func ServiceClientsFromEnv(getenv func(string) string) (ServiceClients, error) {
	value := strings.TrimSpace(getenv(ServiceClientsEnv))
	if value == "" {
		value = DefaultServiceClients
	}
	clients := ServiceClients{}
	if value == "none" {
		return clients, nil
	}
	configured := map[Product]bool{}
	for _, entry := range strings.Split(value, ",") {
		product, client, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || !clientID.MatchString(client) {
			return nil, fmt.Errorf("%w: %q is not product:client", ErrServiceClientsInvalid, entry)
		}
		p := Product(product)
		if !isServiceProduct(p) {
			return nil, fmt.Errorf("%w: %q is not a product with a service account (%v)", ErrServiceClientsInvalid, product, ServiceProducts)
		}
		if configured[p] {
			return nil, fmt.Errorf("%w: %s named twice", ErrServiceClientsInvalid, product)
		}
		if _, taken := clients[client]; taken {
			return nil, fmt.Errorf("%w: client %s named twice", ErrServiceClientsInvalid, client)
		}
		configured[p] = true
		clients[client] = p
	}
	return clients, nil
}

func isServiceProduct(p Product) bool {
	for _, known := range ServiceProducts {
		if p == known {
			return true
		}
	}
	return false
}

// Product is the product whose service account a token's caller is: its
// client's product for a service account's token, and none for a person's,
// whichever client it was issued to.
func (c ServiceClients) Product(client string, serviceAccount bool) Product {
	if !serviceAccount {
		return ""
	}
	return c[client]
}

// Products are the products that have a service client, in ServiceProducts
// order.
func (c ServiceClients) Products() []Product {
	out := []Product{}
	for _, p := range ServiceProducts {
		for _, product := range c {
			if product == p {
				out = append(out, p)
				break
			}
		}
	}
	return out
}
