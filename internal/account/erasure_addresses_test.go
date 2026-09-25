package account_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// keycloakAddresses stands in for the identity provider's view of a person:
// the Primary e-mail and the School and Personal e-mail attributes, raw.
type keycloakAddresses struct {
	addresses []string
	gone      bool
	err       error
	calls     int
}

func (k *keycloakAddresses) UserAddresses(context.Context, uuid.UUID) ([]string, error) {
	k.calls++
	if k.err != nil {
		return nil, k.err
	}
	if k.gone {
		return nil, nil
	}
	return append([]string(nil), k.addresses...), nil
}

type failingCoreUsers struct{}

func (failingCoreUsers) Get(context.Context, uuid.UUID) (user.User, error) {
	return user.User{}, errors.New(`conn closed while reading "ada@example.com"`)
}

func coreUser(t *testing.T, store *user.MemoryStore, primary, school string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, _, err := user.NewService(store).Ensure(context.Background(), id, user.Profile{Email: primary, SchoolEmail: school, FirstName: "Ada"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestErasureAddressesAreTheUnionOfKeycloakAndCoreWithoutRepeats(t *testing.T) {
	t.Parallel()

	const (
		school   = "ada.lovelace@std.yildiz.edu.tr"
		personal = "ada@example.com"
		legacy   = "ada.legacy@example.org"
	)
	for _, tc := range []struct {
		name                    string
		keycloak                []string
		corePrimary, coreSchool string
		want                    []string
	}{
		{
			name:     "two addresses, the School e-mail is primary",
			keycloak: []string{school, school, personal}, corePrimary: school, coreSchool: school,
			want: []string{school, personal},
		},
		{
			name:     "two addresses, the Personal e-mail is primary",
			keycloak: []string{personal, school, personal}, corePrimary: personal, coreSchool: school,
			want: []string{personal, school},
		},
		{
			name:     "School e-mail only",
			keycloak: []string{school, school}, corePrimary: school, coreSchool: school,
			want: []string{school},
		},
		{
			name:     "three addresses, a primary Keycloak kept from before Account center v2",
			keycloak: []string{legacy, school, personal}, corePrimary: legacy, coreSchool: school,
			want: []string{legacy, school, personal},
		},
		{
			name:     "three addresses, core's row still holds the previous primary",
			keycloak: []string{personal, school, personal}, corePrimary: legacy, coreSchool: school,
			want: []string{personal, school, legacy},
		},
		{
			name:     "case and spaces do not make a second address",
			keycloak: []string{" Ada@Example.COM ", "ADA.LOVELACE@std.yildiz.edu.tr", "ada@example.com"}, corePrimary: "ada@EXAMPLE.com", coreSchool: school,
			want: []string{personal, school},
		},
	} {
		store := user.NewMemoryStore()
		id := coreUser(t, store, tc.corePrimary, tc.coreSchool)
		got, err := account.NewErasureAddresses(&keycloakAddresses{addresses: tc.keycloak}, store).ErasureAddresses(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("%s: addresses = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestErasureAddressesFallBackToCoreWhenKeycloakNoLongerKnowsThePerson(t *testing.T) {
	t.Parallel()

	store := user.NewMemoryStore()
	id := coreUser(t, store, "ada@example.com", "ada.lovelace@std.yildiz.edu.tr")
	got, err := account.NewErasureAddresses(&keycloakAddresses{gone: true}, store).ErasureAddresses(context.Background(), id)
	if err != nil || !slices.Equal(got, []string{"ada@example.com", "ada.lovelace@std.yildiz.edu.tr"}) {
		t.Fatalf("addresses = %v err=%v", got, err)
	}
}

func TestErasureAddressesUseKeycloakAloneWithoutACoreRow(t *testing.T) {
	t.Parallel()

	got, err := account.NewErasureAddresses(&keycloakAddresses{addresses: []string{"ada@example.com"}}, user.NewMemoryStore()).
		ErasureAddresses(context.Background(), uuid.New())
	if err != nil || !slices.Equal(got, []string{"ada@example.com"}) {
		t.Fatalf("addresses = %v err=%v", got, err)
	}
}

func TestErasureAddressesFailWithoutNamingAnAddressOrTheSubject(t *testing.T) {
	t.Parallel()

	store := user.NewMemoryStore()
	id := coreUser(t, store, "ada@example.com", "ada.lovelace@std.yildiz.edu.tr")
	for name, source := range map[string]account.ErasureAddressSource{
		// Keycloak unreachable: never go on with core's row alone.
		"keycloak unavailable": account.NewErasureAddresses(&keycloakAddresses{err: errors.New("identity: user address lookup failed")}, store),
		"core row unreadable":  account.NewErasureAddresses(&keycloakAddresses{addresses: []string{"ada@example.com"}}, failingCoreUsers{}),
		"more than three": account.NewErasureAddresses(&keycloakAddresses{addresses: []string{
			"ada@example.com", "ada.lovelace@std.yildiz.edu.tr", "ada.legacy@example.org",
		}}, mustCoreUser(t, "ada.older@example.net")),
	} {
		got, err := source.ErasureAddresses(context.Background(), id)
		if err == nil || got != nil {
			t.Fatalf("%s: addresses = %v err=%v", name, got, err)
		}
		text := strings.ToLower(err.Error())
		for _, value := range []string{"@", id.String()} {
			if strings.Contains(text, value) {
				t.Fatalf("%s: error %q carries %q", name, err, value)
			}
		}
	}
}

// mustCoreUser is a store that answers every id with one row.
func mustCoreUser(t *testing.T, primary string) account.CoreUsers {
	t.Helper()
	return coreUsersFunc(func(context.Context, uuid.UUID) (user.User, error) {
		return user.User{Email: primary}, nil
	})
}

type coreUsersFunc func(context.Context, uuid.UUID) (user.User, error)

func (f coreUsersFunc) Get(ctx context.Context, id uuid.UUID) (user.User, error) { return f(ctx, id) }
