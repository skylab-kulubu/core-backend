package authz_test

import (
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

func getenv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// Unset, only Skyforms' service client is configured: forms-backend requests
// its token as the forms client. The CMS has no service account yet.
func TestServiceClientsDefaultToSkyformsOnly(t *testing.T) {
	t.Parallel()
	clients, err := authz.ServiceClientsFromEnv(getenv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 || clients["forms"] != authz.ProductForms {
		t.Fatalf("clients %v, want forms → forms only", clients)
	}
	if products := clients.Products(); len(products) != 1 || products[0] != authz.ProductForms {
		t.Fatalf("configured products %v, want forms", products)
	}
}

func TestServiceClientsReadProductClientPairs(t *testing.T) {
	t.Parallel()
	clients, err := authz.ServiceClientsFromEnv(getenv(map[string]string{
		"MEDIA_SERVICE_CLIENTS": " forms:forms , cms:cms-core ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 2 || clients["forms"] != authz.ProductForms || clients["cms-core"] != authz.ProductCMS {
		t.Fatalf("clients %v", clients)
	}

	none, err := authz.ServiceClientsFromEnv(getenv(map[string]string{"MEDIA_SERVICE_CLIENTS": "none"}))
	if err != nil || len(none) != 0 {
		t.Fatalf("none: %v, %v", none, err)
	}
}

// A value core cannot read stops it at startup rather than letting the wrong
// client act for a product.
func TestServiceClientsRefuseWhatTheyCannotRead(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		"no client":             "forms",
		"empty client":          "forms:",
		"unknown product":       "arge:arge",
		"core is no service":    "core:core",
		"product twice":         "forms:forms,forms:skyforms",
		"client twice":          "forms:shared,cms:shared",
		"client with a space":   "cms:cms core",
		"empty entry":           "forms:forms,,cms:cms-core",
		"product in upper case": "Forms:forms",
	} {
		_, err := authz.ServiceClientsFromEnv(getenv(map[string]string{"MEDIA_SERVICE_CLIENTS": value}))
		if !errors.Is(err, authz.ErrServiceClientsInvalid) {
			t.Errorf("%s (%q): err = %v, want %v", name, value, err, authz.ErrServiceClientsInvalid)
		}
	}
}

// Only a service account's token names a product, and only through a
// configured client.
func TestServiceClientsResolveOnlyServiceAccounts(t *testing.T) {
	t.Parallel()
	clients := authz.ServiceClients{"forms": authz.ProductForms}
	for name, tc := range map[string]struct {
		client  string
		service bool
		want    authz.Product
	}{
		"forms service account":    {"forms", true, authz.ProductForms},
		"person through forms":     {"forms", false, ""},
		"unconfigured service":     {"skyforms", true, ""},
		"service without a client": {"", true, ""},
	} {
		if got := clients.Product(tc.client, tc.service); got != tc.want {
			t.Errorf("%s: product %q, want %q", name, got, tc.want)
		}
	}
}
