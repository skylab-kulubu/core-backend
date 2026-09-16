package season_test

import (
	"context"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/season"
)

func TestService_PrivilegedCRUDAndPublicRead(t *testing.T) {
	t.Parallel()
	store := season.NewMemoryStore()
	svc := season.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	ctx := context.Background()
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	member := authz.Principal{ID: "m", Groups: []string{"/UYELER/ARGE/WEBLAB"}}

	created, err := svc.Create(ctx, yk, season.Season{Name: "2026-2027", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, created.ID)
	if err != nil || got.Name != "2026-2027" {
		t.Fatalf("get %+v %v", got, err)
	}
	listed, err := svc.List(ctx, false)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list %+v %v", listed, err)
	}
	if _, err := svc.Create(ctx, member, season.Season{Name: "nope"}); !errors.Is(err, season.ErrForbidden) {
		t.Fatalf("member create: %v", err)
	}
	if err := svc.Delete(ctx, yk, created.ID); err != nil {
		t.Fatal(err)
	}
}
