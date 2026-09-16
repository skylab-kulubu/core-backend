package shorturl

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

func TestService_CreateRedirectAndOwnList(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	p := authz.Principal{ID: uid.String(), Roles: []string{"skylapp:access"}}
	created, err := svc.Create(context.Background(), p, "example.com/x", "weblab")
	if err != nil {
		t.Fatal(err)
	}
	if created.URL != "https://example.com/x" || created.Alias != "weblab" {
		t.Fatalf("got %+v", created)
	}
	got, err := svc.Redirect(context.Background(), "weblab")
	if err != nil {
		t.Fatal(err)
	}
	if got.ClickCount != 1 {
		t.Fatalf("clicks %d", got.ClickCount)
	}
	mine, err := svc.ListMine(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 {
		t.Fatalf("mine %d", len(mine))
	}
}

func TestService_StrangerCannotDelete(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:create", "url:delete"}}
	created, err := svc.Create(context.Background(), owner, "https://skylab.com", "")
	if err != nil {
		t.Fatal(err)
	}
	stranger := authz.Principal{ID: "22222222-2222-2222-2222-222222222222", Roles: []string{"url:delete"}}
	if err := svc.Delete(context.Background(), stranger, created.ID); err != ErrForbidden {
		t.Fatalf("got %v", err)
	}
}

func TestService_ModeratorListsAll(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:create"}}
	if _, err := svc.Create(context.Background(), owner, "https://a.test", "a"); err != nil {
		t.Fatal(err)
	}
	mod := authz.Principal{ID: "33333333-3333-3333-3333-333333333333", Roles: []string{"url:moderator"}}
	all, err := svc.ListAll(context.Background(), mod)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("all %d", len(all))
	}
}

func TestService_PrivilegedCreatesWithoutRole(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	p := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Groups: []string{"/UYELER/YK"}}
	if _, err := svc.Create(context.Background(), p, "https://yk.test", "yk"); err != nil {
		t.Fatal(err)
	}
}

func TestService_DuplicateAlias(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	p := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:create"}}
	if _, err := svc.Create(context.Background(), p, "https://a.test", "same"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), p, "https://b.test", "same"); err != ErrConflict {
		t.Fatalf("got %v", err)
	}
}
