package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// The console audit's read (docs/console-audit.md): oldest first from a
// time, by resource type; the author's IP address and Keycloak's details
// are not read; without view-events it is ErrAdminEventsUnavailable.
func TestKeycloakAdminEvents(t *testing.T) {
	f := newAccountKeycloak()
	f.answers["GET /admin-events"] = accountAnswer{200, `[{"id":"e1","time":1764061200000,"realmId":"r1",
		"authDetails":{"realmId":"master","clientId":"c-uuid","userId":"u-1","ipAddress":"203.0.113.9"},
		"operationType":"CREATE","resourceType":"GROUP_MEMBERSHIP","resourcePath":"users/u-2/groups/g-1",
		"representation":"{\"id\":\"g-1\",\"path\":\"/UYELER/YK\"}","details":{"username":"ada"}}]`}
	k := f.serve(t)
	events, err := k.AdminEvents(context.Background(), 1764061199000, 200, []string{"GROUP_MEMBERSHIP", "CLIENT"})
	if err != nil || len(events) != 1 {
		t.Fatalf("events %+v %v", events, err)
	}
	e := events[0]
	if e.ID != "e1" || e.AuthRealmID != "master" || e.AuthClientID != "c-uuid" || e.AuthUserID != "u-1" || e.RealmID != "r1" || e.ResourcePath != "users/u-2/groups/g-1" ||
		!strings.Contains(e.Representation, "/UYELER/YK") {
		t.Fatalf("event %+v", e)
	}
	raw, _ := json.Marshal(events)
	for _, out := range []string{"203.0.113.9", "ada"} {
		if strings.Contains(string(raw), out) {
			t.Fatalf("%q read out: %s", out, raw)
		}
	}
	q := f.last().path
	for _, want := range []string{"dateFrom=1764061199000", "direction=asc", "max=200", "resourceTypes=GROUP_MEMBERSHIP", "resourceTypes=CLIENT"} {
		if !strings.Contains(q, want) {
			t.Fatalf("query %s lacks %s", q, want)
		}
	}
	f.answers["GET /admin-events"] = accountAnswer{403, `{"error":"unknown_error"}`}
	if _, err := k.AdminEvents(context.Background(), 0, 10, nil); !errors.Is(err, identity.ErrAdminEventsUnavailable) {
		t.Fatalf("without view-events: %v", err)
	}
}

// The console audit checks a reconciler's client: confidential (not
// public) and with a service account.
func TestKeycloakClientInfoOf(t *testing.T) {
	f := newAccountKeycloak()
	f.answers["GET /clients"] = accountAnswer{200, `[{"id":"5b0e3c1a-1111-4222-8333-944455556666","clientId":"account-center-config","publicClient":false,"serviceAccountsEnabled":true}]`}
	k := f.serve(t)
	info, err := k.ClientInfoOf(context.Background(), "account-center-config")
	if err != nil || info.ID != "5b0e3c1a-1111-4222-8333-944455556666" || info.Public || !info.ServiceAccounts {
		t.Fatalf("%+v %v", info, err)
	}
	if !strings.Contains(f.last().path, "clientId=account-center-config") {
		t.Fatalf("query %s", f.last().path)
	}
	if _, err := k.ClientInfoOf(context.Background(), "other"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}
