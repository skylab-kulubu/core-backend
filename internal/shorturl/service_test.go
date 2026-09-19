package shorturl

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

func TestService_CreateRedirectAndOwnList(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	p := authz.Principal{ID: uid.String(), Roles: []string{"url:access"}}
	created, err := svc.Create(context.Background(), p, "example.com/x", "weblab")
	if err != nil {
		t.Fatal(err)
	}
	if created.URL != "https://example.com/x" || created.Alias != "weblab" {
		t.Fatalf("got %+v", created)
	}
	got, err := svc.Redirect(context.Background(), "weblab", Hit{})
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

func TestService_RedirectRecordsHitAndListIsNewestFirst(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	svc := NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:create", "url:access"}}
	created, err := svc.Create(context.Background(), owner, "https://skylab.com", "club")
	if err != nil {
		t.Fatal(err)
	}
	uid := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, err := svc.Redirect(context.Background(), "club", Hit{
		IP: "203.0.113.9", UserAgent: "WhatsApp/2.0", Referer: "https://wa.me/",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redirect(context.Background(), "club", Hit{
		IP: "198.51.100.4", UserAgent: "curl/8.0", UserID: &uid,
	}); err != nil {
		t.Fatal(err)
	}
	member := authz.Principal{ID: owner.ID, Roles: []string{"url:access"}}
	if _, err := svc.ListHits(context.Background(), member, created.ID); err != ErrForbidden {
		t.Fatalf("member hits %v", err)
	}
	mod := authz.Principal{ID: "33333333-3333-3333-3333-333333333333", Roles: []string{"url:moderator"}}
	hits, err := svc.ListHits(context.Background(), mod, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits %d", len(hits))
	}
	if hits[0].IP != "198.51.100.4" || hits[0].UserID == nil || *hits[0].UserID != uid {
		t.Fatalf("newest %+v", hits[0])
	}
	if hits[1].IP != "203.0.113.9" || hits[1].UserAgent != "WhatsApp/2.0" || hits[1].Referer != "https://wa.me/" || hits[1].UserID != nil {
		t.Fatalf("anon %+v", hits[1])
	}
	if hits[0].Alias != "club" || hits[1].Alias != "club" {
		t.Fatalf("alias %+v", hits)
	}
	listed, err := svc.ListMine(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ClickCount != 2 {
		t.Fatalf("clicks %+v", listed)
	}
}

func TestService_LookupDoesNotRecordHit(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:create", "url:access"}}
	created, err := svc.Create(context.Background(), owner, "https://skylab.com", "club")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Lookup(context.Background(), "club"); err != nil {
		t.Fatal(err)
	}
	mod := authz.Principal{ID: "33333333-3333-3333-3333-333333333333", Roles: []string{"url:moderator"}}
	hits, err := svc.ListHits(context.Background(), mod, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits %+v", hits)
	}
	mine, err := svc.ListMine(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if mine[0].ClickCount != 0 {
		t.Fatalf("clicks %d", mine[0].ClickCount)
	}
}

func TestService_ListHitsDropsOlderThan90Days(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:create"}}
	created, err := svc.Create(context.Background(), owner, "https://skylab.com", "club")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redirect(context.Background(), "club", Hit{
		IP: "203.0.113.10", CreatedAt: time.Now().UTC().Add(-91 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redirect(context.Background(), "club", Hit{IP: "203.0.113.11"}); err != nil {
		t.Fatal(err)
	}
	mod := authz.Principal{ID: "33333333-3333-3333-3333-333333333333", Roles: []string{"url:moderator"}}
	hits, err := svc.ListHits(context.Background(), mod, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].IP != "203.0.113.11" {
		t.Fatalf("hits %+v", hits)
	}
}

func TestMemoryStore_ListHitsDoesNotDeleteOrRewriteClicks(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	created, err := store.Create(context.Background(), URL{Alias: "club", URL: "https://skylab.com"})
	if err != nil {
		t.Fatal(err)
	}
	old := Hit{
		ID: uuid.New(), URLID: created.ID, Alias: "club",
		CreatedAt: time.Now().UTC().Add(-91 * 24 * time.Hour), IP: "203.0.113.10",
	}
	store.mu.Lock()
	store.hits = append(store.hits, old)
	u := store.byID[created.ID]
	u.ClickCount = 7
	store.byID[created.ID] = u
	store.mu.Unlock()

	hits, err := store.ListHits(context.Background(), created.ID, time.Now().UTC().Add(-HitRetention))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expired should be filtered %+v", hits)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.hits) != 1 {
		t.Fatalf("deleted rows %d", len(store.hits))
	}
	if store.byID[created.ID].ClickCount != 7 {
		t.Fatalf("click_count %d", store.byID[created.ID].ClickCount)
	}
}

func TestService_PrivilegedListsHits(t *testing.T) {
	t.Parallel()
	svc := NewService(NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:create"}}
	created, err := svc.Create(context.Background(), owner, "https://skylab.com", "club")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redirect(context.Background(), "club", Hit{IP: "203.0.113.12"}); err != nil {
		t.Fatal(err)
	}
	yk := authz.Principal{ID: "22222222-2222-2222-2222-222222222222", Groups: []string{"/UYELER/YK"}}
	hits, err := svc.ListHits(context.Background(), yk, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].IP != "203.0.113.12" {
		t.Fatalf("hits %+v", hits)
	}
}
