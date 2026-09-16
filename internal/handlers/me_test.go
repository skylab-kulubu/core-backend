package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func testApp(t *testing.T, store user.Store) *fiber.App {
	t.Helper()
	svc := user.NewService(store)
	jit := middlewares.NewJIT(svc)
	me := NewMeHandler(svc, media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test"))

	app := fiber.New()
	app.Get("/v1/health", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	app.Get("/v1/users/me", jit.Handle, me.GetMe)
	app.Put("/v1/users/me", jit.Handle, me.PutMe)
	app.Patch("/v1/users/me", jit.Handle, me.PatchMe)
	app.Post("/v1/users/me/profile-picture", jit.Handle, me.ProfilePicture)
	return app
}

func TestGetMeUnauthorizedWithoutIdentity(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	app := testApp(t, store)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestHealthDoesNotCreateUser(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	app := testApp(t, store)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/health", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}

	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	_, err = store.Get(t.Context(), id)
	if err != user.ErrNotFound {
		t.Fatalf("expected no user, got %v", err)
	}
}

func TestGetMeUpsertsIdentity(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := user.NewService(store)
	jit := middlewares.NewJIT(svc)
	me := NewMeHandler(svc, nil)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{
			ID: id,
			Profile: user.Profile{
				Email:     "ada@example.com",
				FirstName: "Ada",
				LastName:  "Lovelace",
			},
		})
		return c.Next()
	})
	app.Get("/v1/users/me", jit.Handle, me.GetMe)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}

	got, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "ada@example.com" || got.SkyNumber != "SKY-0000001" {
		t.Fatalf("got %+v", got)
	}
}

func TestGetMeReturnsSchoolEmail(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := user.NewService(store)
	jit := middlewares.NewJIT(svc)
	me := NewMeHandler(svc, nil)
	id := uuid.MustParse("66666666-6666-6666-6666-666666666666")

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{
			ID: id,
			Profile: user.Profile{
				Email:       "ada@example.com",
				FirstName:   "Ada",
				LastName:    "Lovelace",
				SchoolEmail: "ada@std.yildiz.edu.tr",
			},
		})
		return c.Next()
	})
	app.Get("/v1/users/me", jit.Handle, me.GetMe)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	got, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchoolEmail != "ada@std.yildiz.edu.tr" {
		t.Fatalf("got %+v", got)
	}
}

type recMail struct {
	n int
}

func (r *recMail) Welcome(_ context.Context, _ user.User) {
	r.n++
}

func TestGetMeWelcomeOnlyOnCreate(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	svc := user.NewService(store)
	rec := &recMail{}
	jit := middlewares.NewJIT(svc, rec)
	me := NewMeHandler(svc, nil)
	id := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{
			ID:      id,
			Profile: user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"},
		})
		return c.Next()
	})
	app.Get("/v1/users/me", jit.Handle, me.GetMe)

	for i := 0; i < 2; i++ {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if rec.n != 1 {
		t.Fatalf("welcome count %d", rec.n)
	}
}

func meIdentApp(t *testing.T, store user.Store, id uuid.UUID, profile user.Profile, mediaSvc media.Service) *fiber.App {
	t.Helper()
	svc := user.NewService(store)
	jit := middlewares.NewJIT(svc)
	me := NewMeHandler(svc, mediaSvc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: id, Profile: profile})
		return c.Next()
	})
	app.Get("/v1/users/me", jit.Handle, me.GetMe)
	app.Put("/v1/users/me", jit.Handle, me.PutMe)
	app.Patch("/v1/users/me", jit.Handle, me.PatchMe)
	app.Post("/v1/users/me/profile-picture", jit.Handle, me.ProfilePicture)
	return app
}

func TestPutAndPatchMeHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	profile := user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", SchoolEmail: "ada@std.yildiz.edu.tr", Username: "ada"}
	app := meIdentApp(t, store, id, profile, nil)

	req := httptest.NewRequest(fiber.MethodPut, "/v1/users/me", strings.NewReader(
		`{"firstName":"Ada","lastName":"Byron","linkedin":"https://linkedin.com/in/ada","university":"YTÜ","faculty":"EE","department":"CE"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("put status %d body %s", resp.StatusCode, body)
	}
	var put user.User
	if err := json.NewDecoder(resp.Body).Decode(&put); err != nil {
		t.Fatal(err)
	}
	if put.LastName != "Byron" || put.Linkedin == "" || put.SkyNumber != "SKY-0000001" || put.SchoolEmail != "ada@std.yildiz.edu.tr" || put.Username != "ada" {
		t.Fatalf("put %+v", put)
	}

	patchReq := httptest.NewRequest(fiber.MethodPatch, "/v1/users/me", strings.NewReader(`{"department":"CS"}`))
	patchReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch status %d body %s", resp.StatusCode, body)
	}
	var patched user.User
	if err := json.NewDecoder(resp.Body).Decode(&patched); err != nil {
		t.Fatal(err)
	}
	if patched.Department != "CS" || patched.University != "YTÜ" || patched.SkyNumber != "SKY-0000001" {
		t.Fatalf("patched %+v", patched)
	}

	emptyReq := httptest.NewRequest(fiber.MethodPut, "/v1/users/me", strings.NewReader(`{"firstName":"Ada","lastName":"Byron"}`))
	emptyReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(emptyReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("empty put %d", resp.StatusCode)
	}
	var cleared user.User
	if err := json.NewDecoder(resp.Body).Decode(&cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Linkedin != "" || cleared.SkyNumber != "SKY-0000001" || cleared.SchoolEmail != "ada@std.yildiz.edu.tr" {
		t.Fatalf("cleared %+v", cleared)
	}
}

func TestProfilePictureHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	mediaSvc := media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	app := meIdentApp(t, store, id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}, mediaSvc)

	body, ctype := multipartPNG(t, "image", "dot.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/users/me/profile-picture", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	var got user.User
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ProfilePictureURL == "" || got.ProfilePictureID == nil {
		t.Fatalf("picture %+v", got)
	}
	if !strings.HasPrefix(got.ProfilePictureURL, "https://cdn.example.test/") {
		t.Fatalf("url %s", got.ProfilePictureURL)
	}
}

func TestPutMeUnauthorized(t *testing.T) {
	t.Parallel()
	app := testApp(t, user.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPut, "/v1/users/me", bytes.NewReader([]byte(`{"firstName":"Ada","lastName":"Lovelace"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
