package shorturl_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
)

// checkGoneHops is shared by the memory and Postgres stores: a disabled link's
// alias and the spelling it was renamed away from are gone, an alias that
// never existed and a case variant of a disabled one are only not found, and
// both answers are still ErrNotFound for every caller that checks only that.
func checkGoneHops(t *testing.T, store shorturl.Store, owner authz.Principal) {
	t.Helper()
	ctx := context.Background()
	svc := shorturl.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	created, err := svc.Create(ctx, owner, "https://skylab.com", "club")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, "", "kulup"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redirect(ctx, "club", shorturl.Hit{}); err != nil {
		t.Fatalf("retired alias of a live link: %v", err)
	}
	if err := svc.Delete(ctx, owner, created.ID); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"kulup", "club"} {
		if _, err := svc.Redirect(ctx, alias, shorturl.Hit{}); !errors.Is(err, shorturl.ErrGone) || !errors.Is(err, shorturl.ErrNotFound) {
			t.Fatalf("%s: %v", alias, err)
		}
	}
	for _, alias := range []string{"missing", "KULUP"} {
		if _, err := svc.Redirect(ctx, alias, shorturl.Hit{}); !errors.Is(err, shorturl.ErrNotFound) || errors.Is(err, shorturl.ErrGone) {
			t.Fatalf("%s: %v", alias, err)
		}
	}
	if _, err := svc.Restore(ctx, owner, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redirect(ctx, "kulup", shorturl.Hit{}); err != nil {
		t.Fatalf("restored link: %v", err)
	}
}

func TestDisabledLinkIsGoneOnMemory(t *testing.T) {
	owner := authz.Principal{ID: uuid.NewString(), Roles: []string{"url:access"}}
	checkGoneHops(t, shorturl.NewMemoryStore(), owner)
}
