package shorturl_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestDisabledLinkIsGoneOnPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ownerID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, ownerID, user.Profile{Email: "owner@example.test"}); err != nil {
		t.Fatal(err)
	}
	owner := authz.Principal{ID: ownerID.String(), Roles: []string{"url:access"}}
	checkGoneHops(t, shorturl.NewPostgresStore(pool), owner)
}
