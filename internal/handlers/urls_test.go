package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
)

type pruneErrStore struct {
	shorturl.Store
	err error
}

func (s *pruneErrStore) RecordHit(ctx context.Context, id uuid.UUID, hit shorturl.Hit) (shorturl.URL, error) {
	u, err := s.Store.RecordHit(ctx, id, hit)
	if err != nil {
		return u, err
	}
	return u, s.err
}

func urlApp(t *testing.T, ident authn.Identity) *fiber.App {
	t.Helper()
	return urlAppOn(t, shorturl.NewMemoryStore(), ident, nil)
}

func urlAppWith(t *testing.T, ident authn.Identity, store shorturl.Store) *fiber.App {
	t.Helper()
	return urlAppOn(t, store, ident, nil)
}

func urlAppOn(t *testing.T, store shorturl.Store, ident authn.Identity, parse func(string) (authn.Identity, error)) *fiber.App {
	t.Helper()
	h := NewURLHandler(shorturl.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy())), parse)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Roles) > 0 || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Get("/v1/go/:alias/qr", h.QR)
	app.Get("/v1/go/:alias", h.Redirect)
	app.Post("/v1/urls", h.Create)
	app.Get("/v1/urls", h.ListMine)
	app.Get("/v1/urls/all", h.ListAll)
	app.Get("/v1/urls/:id/hits", h.ListHits)
	app.Patch("/v1/urls/:id", h.Update)
	app.Delete("/v1/urls/:id", h.Delete)
	return app
}

func TestURLSkylappRoleDoesNotCreate(t *testing.T) {
	t.Parallel()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlApp(t, authn.Identity{ID: uid, Roles: []string{"skylapp:access", "skylapp:url:create"}})
	req := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestURLCreateRedirectAndListHTTP(t *testing.T) {
	t.Parallel()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlApp(t, authn.Identity{ID: uid, Roles: []string{"url:access"}})
	req := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	redir, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil))
	if err != nil {
		t.Fatal(err)
	}
	if redir.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("redirect %d", redir.StatusCode)
	}
	if loc := redir.Header.Get("Location"); loc != "https://skylab.com" {
		t.Fatalf("location %s", loc)
	}
	list, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls", nil))
	if err != nil {
		t.Fatal(err)
	}
	if list.StatusCode != fiber.StatusOK {
		t.Fatalf("list %d", list.StatusCode)
	}
	var items []shorturl.URL
	if err := json.NewDecoder(list.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ClickCount != 1 {
		t.Fatalf("items %+v", items)
	}
}

func TestURLQRDoesNotIncrementClicks(t *testing.T) {
	t.Parallel()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlApp(t, authn.Identity{ID: uid, Roles: []string{"url:access"}})
	req := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}

	qrResp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/club/qr", nil))
	if err != nil {
		t.Fatal(err)
	}
	if qrResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(qrResp.Body)
		t.Fatalf("qr status %d body %s", qrResp.StatusCode, b)
	}
	if ct := qrResp.Header.Get("Content-Type"); !strings.Contains(ct, "image/png") {
		t.Fatalf("content-type %s", ct)
	}
	body, err := io.ReadAll(qrResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 8 || string(body[:4]) != "\x89PNG" {
		t.Fatalf("not png len=%d", len(body))
	}

	missing, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/nope/qr", nil))
	if err != nil {
		t.Fatal(err)
	}
	if missing.StatusCode != fiber.StatusNotFound {
		t.Fatalf("missing %d", missing.StatusCode)
	}

	list, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls", nil))
	if err != nil {
		t.Fatal(err)
	}
	var items []shorturl.URL
	if err := json.NewDecoder(list.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ClickCount != 0 {
		t.Fatalf("clicks %+v", items)
	}
}

func TestURLQRWithLogoDoesNotIncrementClicks(t *testing.T) {
	t.Parallel()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlApp(t, authn.Identity{ID: uid, Roles: []string{"url:access"}})
	req := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}

	qrResp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/club/qr?logo=1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if qrResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(qrResp.Body)
		t.Fatalf("qr status %d body %s", qrResp.StatusCode, b)
	}
	body, err := io.ReadAll(qrResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 8 || string(body[:4]) != "\x89PNG" {
		t.Fatalf("not png len=%d", len(body))
	}

	missing, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/nope/qr?logo=1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if missing.StatusCode != fiber.StatusNotFound {
		t.Fatalf("missing %d", missing.StatusCode)
	}

	list, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls", nil))
	if err != nil {
		t.Fatal(err)
	}
	var items []shorturl.URL
	if err := json.NewDecoder(list.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ClickCount != 0 {
		t.Fatalf("clicks %+v", items)
	}
}

func TestURLListAllForbiddenForMember(t *testing.T) {
	t.Parallel()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlApp(t, authn.Identity{ID: uid, Roles: []string{"url:create"}})
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls/all", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func createClubURL(t *testing.T, store shorturl.Store) shorturl.URL {
	t.Helper()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlAppOn(t, store, authn.Identity{ID: uid, Roles: []string{"url:access"}}, nil)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	var created shorturl.URL
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return created
}

func TestURLRedirectStays301WhenHitPruneFails(t *testing.T) {
	t.Parallel()
	store := &pruneErrStore{Store: shorturl.NewMemoryStore(), err: errors.New("prune failed")}
	created := createClubURL(t, store)
	anon := urlAppOn(t, store, authn.Identity{}, nil)
	req := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.20")
	req.Header.Set("User-Agent", "WhatsApp/2.0")
	req.Header.Set("Referer", "https://wa.me/invite")
	redir, err := anon.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if redir.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("redirect %d", redir.StatusCode)
	}
	if loc := redir.Header.Get("Location"); loc != "https://skylab.com" {
		t.Fatalf("location %s", loc)
	}

	mod := urlAppOn(t, store, authn.Identity{
		ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Roles: []string{"url:moderator"},
	}, nil)
	list, err := mod.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls/"+created.ID.String()+"/hits", nil))
	if err != nil {
		t.Fatal(err)
	}
	if list.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(list.Body)
		t.Fatalf("hits %d body %s", list.StatusCode, b)
	}
	var hits []shorturl.Hit
	if err := json.NewDecoder(list.Body).Decode(&hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	got := hits[0]
	if got.Alias != "club" || got.IP != "198.51.100.20" || got.UserAgent != "WhatsApp/2.0" || got.Referer != "https://wa.me/invite" {
		t.Fatalf("hit %+v", got)
	}
	if got.UserID != nil {
		t.Fatalf("public click userId %v", got.UserID)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("missing createdAt")
	}
}

func TestGoRedirectInsertsHitWithoutKeycloak(t *testing.T) {
	t.Parallel()
	store := shorturl.NewMemoryStore()
	created := createClubURL(t, store)
	anon := urlAppOn(t, store, authn.Identity{}, nil)
	req := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.20")
	req.Header.Set("User-Agent", "WhatsApp/2.0")
	req.Header.Set("Referer", "https://wa.me/invite")
	redir, err := anon.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if redir.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("redirect %d", redir.StatusCode)
	}
	loc := redir.Header.Get("Location")
	if loc != "https://skylab.com" {
		t.Fatalf("location %s", loc)
	}
	if strings.Contains(strings.ToLower(loc), "keycloak") || strings.Contains(loc, "/realms/") {
		t.Fatalf("sso hop %s", loc)
	}

	mod := urlAppOn(t, store, authn.Identity{
		ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Roles: []string{"url:moderator"},
	}, nil)
	list, err := mod.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls/"+created.ID.String()+"/hits", nil))
	if err != nil {
		t.Fatal(err)
	}
	if list.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(list.Body)
		t.Fatalf("hits %d body %s", list.StatusCode, b)
	}
	raw, err := io.ReadAll(list.Body)
	if err != nil {
		t.Fatal(err)
	}
	var wire []map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 1 {
		t.Fatalf("wire %+v", wire)
	}
	row := wire[0]
	if row["ip"] != "198.51.100.20" || row["userAgent"] != "WhatsApp/2.0" || row["referer"] != "https://wa.me/invite" || row["alias"] != "club" {
		t.Fatalf("wire %+v", row)
	}
	if _, ok := row["createdAt"]; !ok {
		t.Fatal("missing createdAt")
	}
	if uid, ok := row["userId"]; ok && uid != nil && uid != "" {
		t.Fatalf("userId %v", uid)
	}
	var hits []shorturl.Hit
	if err := json.Unmarshal(raw, &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	got := hits[0]
	if got.Alias != "club" || got.IP != "198.51.100.20" || got.UserAgent != "WhatsApp/2.0" || got.Referer != "https://wa.me/invite" {
		t.Fatalf("hit %+v", got)
	}
	if got.UserID != nil {
		t.Fatalf("public click userId %v", got.UserID)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("missing createdAt")
	}
}

func TestHitsListAuthPrivilegedOrModerator(t *testing.T) {
	t.Parallel()
	store := shorturl.NewMemoryStore()
	created := createClubURL(t, store)
	path := "/v1/urls/" + created.ID.String() + "/hits"

	anon := urlAppOn(t, store, authn.Identity{}, nil)
	resp, err := anon.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anon %d", resp.StatusCode)
	}

	member := urlAppOn(t, store, authn.Identity{
		ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Roles: []string{"url:create", "url:access"},
	}, nil)
	resp, err = member.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("member %d", resp.StatusCode)
	}

	yk := urlAppOn(t, store, authn.Identity{
		ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Groups: []string{"/UYELER/YK"},
	}, nil)
	resp, err = yk.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("privileged %d body %s", resp.StatusCode, b)
	}

	mod := urlAppOn(t, store, authn.Identity{
		ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Roles: []string{"url:moderator"},
	}, nil)
	resp, err = mod.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("moderator %d body %s", resp.StatusCode, b)
	}
}

func TestURLRedirectRecordsSilentHit(t *testing.T) {
	t.Parallel()
	store := shorturl.NewMemoryStore()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlAppOn(t, store, authn.Identity{ID: uid, Roles: []string{"url:access"}}, nil)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	var created shorturl.URL
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	hop := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	hop.Header.Set("User-Agent", "WhatsApp/2.0")
	hop.Header.Set("Referer", "https://wa.me/")
	hop.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	redir, err := urlAppOn(t, store, authn.Identity{}, nil).Test(hop)
	if err != nil {
		t.Fatal(err)
	}
	if redir.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("redirect %d", redir.StatusCode)
	}

	memberHits, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls/"+created.ID.String()+"/hits", nil))
	if err != nil {
		t.Fatal(err)
	}
	if memberHits.StatusCode != fiber.StatusForbidden {
		t.Fatalf("member hits %d", memberHits.StatusCode)
	}

	mod := urlAppOn(t, store, authn.Identity{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Roles: []string{"url:moderator"}}, nil)
	hitsResp, err := mod.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls/"+created.ID.String()+"/hits", nil))
	if err != nil {
		t.Fatal(err)
	}
	if hitsResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(hitsResp.Body)
		t.Fatalf("hits %d %s", hitsResp.StatusCode, b)
	}
	var hits []shorturl.Hit
	if err := json.NewDecoder(hitsResp.Body).Decode(&hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	if hits[0].IP != "203.0.113.9" || hits[0].UserAgent != "WhatsApp/2.0" || hits[0].Referer != "https://wa.me/" || hits[0].UserID != nil || hits[0].Alias != "club" {
		t.Fatalf("hit %+v", hits[0])
	}

	list, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls", nil))
	if err != nil {
		t.Fatal(err)
	}
	var items []shorturl.URL
	if err := json.NewDecoder(list.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ClickCount != 1 {
		t.Fatalf("clicks %+v", items)
	}
}

func TestURLQRDoesNotInsertHits(t *testing.T) {
	t.Parallel()
	store := shorturl.NewMemoryStore()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := urlAppOn(t, store, authn.Identity{ID: uid, Roles: []string{"url:access"}}, nil)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	var created shorturl.URL
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/club/qr?logo=1", nil)); err != nil {
		t.Fatal(err)
	}
	mod := urlAppOn(t, store, authn.Identity{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Roles: []string{"url:moderator"}}, nil)
	hitsResp, err := mod.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls/"+created.ID.String()+"/hits", nil))
	if err != nil {
		t.Fatal(err)
	}
	var hits []shorturl.Hit
	if err := json.NewDecoder(hitsResp.Body).Decode(&hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits %+v", hits)
	}
}

func TestURLRedirectAttachesUserFromBearerNotCookies(t *testing.T) {
	t.Parallel()
	store := shorturl.NewMemoryStore()
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	clicker := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	parse := func(token string) (authn.Identity, error) {
		if token == "good-bearer" || token == "good-cookie" {
			return authn.Identity{ID: clicker}, nil
		}
		return authn.Identity{}, authn.ErrInvalidToken
	}
	app := urlAppOn(t, store, authn.Identity{ID: uid, Roles: []string{"url:access"}}, parse)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	var created shorturl.URL
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	hop := urlAppOn(t, store, authn.Identity{}, parse)
	bad := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	badResp, err := hop.Test(bad)
	if err != nil {
		t.Fatal(err)
	}
	if badResp.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("invalid bearer %d", badResp.StatusCode)
	}

	bearer := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	bearer.Header.Set("Authorization", "Bearer good-bearer")
	if _, err := hop.Test(bearer); err != nil {
		t.Fatal(err)
	}

	cookie := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	cookie.AddCookie(&http.Cookie{Name: "access_token", Value: "good-cookie"})
	if _, err := hop.Test(cookie); err != nil {
		t.Fatal(err)
	}

	authCookie := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	authCookie.AddCookie(&http.Cookie{Name: "auth_token", Value: "good-cookie"})
	if _, err := hop.Test(authCookie); err != nil {
		t.Fatal(err)
	}

	mod := urlAppOn(t, store, authn.Identity{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Groups: []string{"/UYELER/YK"}}, parse)
	hitsResp, err := mod.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls/"+created.ID.String()+"/hits", nil))
	if err != nil {
		t.Fatal(err)
	}
	var hits []shorturl.Hit
	if err := json.NewDecoder(hitsResp.Body).Decode(&hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 4 {
		t.Fatalf("hits %+v", hits)
	}
	if hits[0].UserID != nil {
		t.Fatalf("auth_token cookie hit %+v", hits[0])
	}
	if hits[1].UserID != nil {
		t.Fatalf("access_token cookie hit %+v", hits[1])
	}
	if hits[2].UserID == nil || *hits[2].UserID != clicker {
		t.Fatalf("bearer hit %+v", hits[2])
	}
	if hits[3].UserID != nil {
		t.Fatalf("invalid bearer should stay anonymous %+v", hits[3])
	}
}
