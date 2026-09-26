package shorturl_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestRenamedAliasKeepsRedirectingOnPostgres(t *testing.T) {
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
	svc := shorturl.NewService(shorturl.NewPostgresStore(pool), authz.NewAuthorizer(authz.DefaultPolicy()))

	created, err := svc.Create(ctx, owner, "https://skylab.com", "club")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, "", "kulup"); err != nil {
		t.Fatal(err)
	}
	if old, err := svc.Redirect(ctx, "club", shorturl.Hit{}); err != nil || old.ID != created.ID || old.Alias != "kulup" {
		t.Fatalf("old alias: %+v %v", old, err)
	}
	for _, alias := range []string{"club", "CLUB", "Kulup"} {
		if _, err := svc.Create(ctx, owner, "https://example.com", alias); !errors.Is(err, shorturl.ErrConflict) {
			t.Fatalf("%s: %v", alias, err)
		}
	}
	if back, err := svc.Update(ctx, owner, created.ID, "", "club"); err != nil || back.Alias != "club" {
		t.Fatalf("take the old alias back: %+v %v", back, err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, "", "kulup"); err != nil {
		t.Fatalf("retire the same alias twice: %v", err)
	}
	if _, err := svc.Lookup(ctx, "club"); err != nil {
		t.Fatalf("old alias after a second rename: %v", err)
	}
}

func TestCaseOnlyRenameKeepsTheOldSpellingOnPostgres(t *testing.T) {
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
	svc := shorturl.NewService(shorturl.NewPostgresStore(pool), authz.NewAuthorizer(authz.DefaultPolicy()))

	created, err := svc.Create(ctx, owner, "https://skylab.com", "GeceKodu")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, "", "gecekodu"); err != nil {
		t.Fatal(err)
	}
	if old, err := svc.Lookup(ctx, "GeceKodu"); err != nil || old.ID != created.ID {
		t.Fatalf("a printed GeceKodu must keep working after a case-only rename: %+v %v", old, err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, "", "kodgecesi"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, "", "GECEKODU"); err != nil {
		t.Fatalf("take an old alias back in another case: %v", err)
	}
	for _, alias := range []string{"GeceKodu", "gecekodu", "kodgecesi", "GECEKODU"} {
		if got, err := svc.Lookup(ctx, alias); err != nil || got.ID != created.ID {
			t.Fatalf("%s: %+v %v", alias, got, err)
		}
	}
}
