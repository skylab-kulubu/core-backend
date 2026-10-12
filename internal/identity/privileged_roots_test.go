package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

type groupGetter func(ctx context.Context, idOrPath string) (identity.Group, error)

func (f groupGetter) GetGroup(ctx context.Context, idOrPath string) (identity.Group, error) {
	return f(ctx, idOrPath)
}

func TestPrivilegedRootsLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "11111111-1111-1111-1111-111111111111", Name: "ADMIN", Path: "/ADMIN"})
	dir.PutGroup(identity.Group{ID: "22222222-2222-2222-2222-222222222222", Name: "YK", Path: "/UYELER/YK"})
	// Not a root: a Group of the same name elsewhere.
	dir.PutGroup(identity.Group{ID: "33333333-3333-3333-3333-333333333333", Name: "DK", Path: "/UYELER/ARGE/DK"})

	line := identity.PrivilegedRootsLine(ctx, dir, authz.DefaultPrivilegedRoots())
	want := "authz privileged roots (AUTHZ_PRIVILEGED_ROOTS) on Keycloak: " +
		"found /ADMIN (11111111-1111-1111-1111-111111111111), /UYELER/YK (22222222-2222-2222-2222-222222222222); " +
		"missing /YK, /DK, /UYELER/ADMIN, /UYELER/DK; nobody is Privileged through a missing root until a Group is created at its path"
	if line != want {
		t.Fatalf("line\n%s\nwant\n%s", line, want)
	}

	line = identity.PrivilegedRootsLine(ctx, dir, []string{"/ADMIN"})
	if !strings.HasSuffix(line, "found /ADMIN (11111111-1111-1111-1111-111111111111); missing none") {
		t.Fatalf("line %s", line)
	}

	// A lookup that answers another Group (Keycloak's search falls back to
	// names) is a missing root.
	other := groupGetter(func(context.Context, string) (identity.Group, error) {
		return identity.Group{ID: "x", Name: "YK", Path: "/UYELER/ARGE/YK"}, nil
	})
	if line := identity.PrivilegedRootsLine(ctx, other, []string{"/YK"}); !strings.Contains(line, "found none; missing /YK") {
		t.Fatalf("line %s", line)
	}

	down := groupGetter(func(context.Context, string) (identity.Group, error) {
		return identity.Group{}, errors.New("identity: keycloak answered 503")
	})
	if line := identity.PrivilegedRootsLine(ctx, down, authz.DefaultPrivilegedRoots()); line !=
		"authz privileged roots (AUTHZ_PRIVILEGED_ROOTS) could not be checked on Keycloak: identity: keycloak answered 503" {
		t.Fatalf("line %s", line)
	}
}
