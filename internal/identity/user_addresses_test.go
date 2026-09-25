package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// fakeKeycloakUser serves one user's admin representation, a 404 once the
// user is gone, or a failure that writes personal data back.
type fakeKeycloakUser struct {
	mu             sync.Mutex
	id             uuid.UUID
	representation map[string]any
	status         int
	methods        []string
}

func (f *fakeKeycloakUser) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.methods = append(f.methods, r.Method)
		if r.URL.Path != "/admin/realms/e-skylab/users/"+f.id.String() {
			http.NotFound(w, r)
			return
		}
		switch {
		case f.status != 0:
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":"` + f.id.String() + ` kisi@example.com"}`))
		case f.representation == nil:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"User not found"}`))
		default:
			_ = json.NewEncoder(w).Encode(f.representation)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAddressDirectory(url string) *identity.Keycloak {
	return identity.NewKeycloak(identity.KeycloakConfig{URL: url, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

func TestKeycloakUserAddressesReadsThePrimaryAndBothAddressAttributesReadOnly(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	fake := &fakeKeycloakUser{id: id, representation: map[string]any{
		"id": id.String(), "username": "ada", "enabled": false,
		"email": "Ada.Lovelace@std.yildiz.edu.tr",
		"attributes": map[string][]string{
			"schoolEmail":   {"ada.lovelace@std.yildiz.edu.tr"},
			"personalEmail": {"ada@example.com"},
			"skyNumber":     {"SKY-0000042"},
			"phone":         {"+90 555 000 00 00"},
		},
	}}
	srv := fake.serve(t)

	got, err := identity.NewAccountIdentity(newAddressDirectory(srv.URL)).UserAddresses(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Raw values: trimming, lower-casing and dropping repeats is the
	// caller's job, together with core's own row.
	want := []string{"Ada.Lovelace@std.yildiz.edu.tr", "ada.lovelace@std.yildiz.edu.tr", "ada@example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	if !slices.Equal(fake.methods, []string{http.MethodGet}) {
		t.Fatalf("the address read is not read-only: %v", fake.methods)
	}
}

func TestKeycloakUserAddressesSkipsWhatThePersonDoesNotHave(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	fake := &fakeKeycloakUser{id: id, representation: map[string]any{
		"id": id.String(), "username": "grace", "email": "grace@example.com",
		"attributes": map[string][]string{"personalEmail": {"grace@example.com"}, "schoolEmail": {""}},
	}}
	srv := fake.serve(t)

	got, err := newAddressDirectory(srv.URL).UserAddresses(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"grace@example.com", "grace@example.com"}; !slices.Equal(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
}

func TestKeycloakUserAddressesOfAMissingUserAreNoneForTheLifecycle(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	srv := (&fakeKeycloakUser{id: id}).serve(t)
	directory := newAddressDirectory(srv.URL)

	if _, err := directory.UserAddresses(context.Background(), id); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("directory error = %v, want ErrNotFound", err)
	}
	// The erasure saga then goes on with core's own row alone.
	got, err := identity.NewAccountIdentity(directory).UserAddresses(context.Background(), id)
	if err != nil || len(got) != 0 {
		t.Fatalf("lifecycle addresses = %v err=%v", got, err)
	}
}

func TestKeycloakUserAddressesFailureCarriesNoSubjectOrAddress(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	fake := &fakeKeycloakUser{id: id, status: http.StatusInternalServerError}
	srv := fake.serve(t)
	lifecycle := identity.NewAccountIdentity(newAddressDirectory(srv.URL))

	_, err := lifecycle.UserAddresses(context.Background(), id)
	if err == nil || errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("500 error = %v", err)
	}
	assertNoSubjectOrAddress(t, err, id)

	// A transport failure names the URL, and the URL names the subject.
	srv.Close()
	_, err = lifecycle.UserAddresses(context.Background(), id)
	if err == nil || errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unreachable error = %v", err)
	}
	assertNoSubjectOrAddress(t, err, id)
}

func TestAccountIdentityRefusesADirectoryThatCannotReadAddresses(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	memory := identity.NewMemory()
	memory.PutUser(identity.Person{ID: id, Email: "ada@example.com"})
	if _, err := identity.NewAccountIdentity(memory).UserAddresses(context.Background(), id); err == nil {
		t.Fatal("the development directory answered for the identity provider")
	}
}

func assertNoSubjectOrAddress(t *testing.T, err error, id uuid.UUID) {
	t.Helper()
	text := strings.ToLower(err.Error())
	for _, value := range []string{id.String(), "kisi@example.com", "/users/"} {
		if strings.Contains(text, value) {
			t.Fatalf("error %q carries %q", err, value)
		}
	}
}
