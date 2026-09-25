package handlers

import (
	"bytes"
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
	app.Delete("/v1/users/me/profile-picture", jit.Handle, me.DeleteProfilePicture)
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
	app.Delete("/v1/users/me/profile-picture", jit.Handle, me.DeleteProfilePicture)
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

// seedOwnPhone stores an admin-written phone on the shadow the way
// PATCH /v1/users/:id does, without going through the identity service.
func seedOwnPhone(t *testing.T, store user.Store, id uuid.UUID, phone string) {
	t.Helper()
	if _, _, err := store.Upsert(t.Context(), user.User{ID: id, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", SkyNumber: "SKY-0000001"}); err != nil {
		t.Fatal(err)
	}
	existing, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	existing.Phone = phone
	if _, err := store.UpdateProfile(t.Context(), existing); err != nil {
		t.Fatal(err)
	}
}

func decodeJSONMap(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMeResponsesIncludeOwnPhoneHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.MustParse("99999999-9999-9999-9999-999999999991")
	const phone = "+905551234567"
	seedOwnPhone(t, store, id, phone)
	mediaSvc := media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	app := meIdentApp(t, store, id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}, mediaSvc)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("get status %d body %s", resp.StatusCode, b)
	}
	me := decodeJSONMap(t, resp)
	if me["phone"] != phone {
		t.Fatalf("own phone missing on /me: %+v", me)
	}
	if me["id"] != id.String() || me["skyNumber"] != "SKY-0000001" || me["studentCardLinked"] != false {
		t.Fatalf("me lost its fields %+v", me)
	}
	if _, ok := me["studentCardUid"]; ok {
		t.Fatalf("studentCardUid on /me: %+v", me)
	}

	putReq := httptest.NewRequest(fiber.MethodPut, "/v1/users/me", strings.NewReader(`{"firstName":"Ada","lastName":"Byron","department":"CE"}`))
	putReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(putReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("put status %d body %s", resp.StatusCode, b)
	}
	if put := decodeJSONMap(t, resp); put["phone"] != phone || put["lastName"] != "Byron" {
		t.Fatalf("put %+v", put)
	}

	// The person may read the number but not write it before verification exists.
	patchReq := httptest.NewRequest(fiber.MethodPatch, "/v1/users/me", strings.NewReader(`{"department":"CS","phone":"+900000000000"}`))
	patchReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch status %d body %s", resp.StatusCode, b)
	}
	if patched := decodeJSONMap(t, resp); patched["phone"] != phone || patched["department"] != "CS" {
		t.Fatalf("self patch changed phone %+v", patched)
	}
	stored, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phone != phone {
		t.Fatalf("self patch wrote phone %q", stored.Phone)
	}

	body, ctype := multipartPNG(t, "image", "dot.png", pngDotHTTP())
	picReq := httptest.NewRequest(fiber.MethodPost, "/v1/users/me/profile-picture", body)
	picReq.Header.Set("Content-Type", ctype)
	resp, err = app.Test(picReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("picture status %d body %s", resp.StatusCode, b)
	}
	if pic := decodeJSONMap(t, resp); pic["phone"] != phone || pic["profilePictureUrl"] == nil {
		t.Fatalf("picture %+v", pic)
	}
}

func TestGetMeOmitsPhoneKeyWhenUnset(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.MustParse("99999999-9999-9999-9999-999999999992")
	app := meIdentApp(t, store, id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}, nil)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"phone"`) {
		t.Fatalf("empty phone serialized: %s", raw)
	}
}

func TestDeleteProfilePictureHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.MustParse("99999999-9999-9999-9999-999999999993")
	mediaStore := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	mediaSvc := media.NewService(mediaStore, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	app := meIdentApp(t, store, id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}, mediaSvc)

	// Removing a picture that was never set is already a success.
	resp, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/v1/users/me/profile-picture", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete without picture status %d body %s", resp.StatusCode, b)
	}

	body, ctype := multipartPNG(t, "image", "dot.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/users/me/profile-picture", body)
	req.Header.Set("Content-Type", ctype)
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status %d body %s", resp.StatusCode, b)
	}
	var uploaded user.User
	if err := json.NewDecoder(resp.Body).Decode(&uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.ProfilePictureID == nil {
		t.Fatalf("upload %+v", uploaded)
	}
	pictureID := *uploaded.ProfilePictureID
	picture, err := mediaStore.Get(t.Context(), pictureID)
	if err != nil {
		t.Fatal(err)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodDelete, "/v1/users/me/profile-picture", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete status %d body %s", resp.StatusCode, b)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "profilePictureUrl") || strings.Contains(string(raw), "profilePictureId") {
		t.Fatalf("picture still on /me: %s", raw)
	}
	stored, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProfilePictureID != nil || stored.ProfilePictureURL != "" {
		t.Fatalf("shadow still linked %+v", stored)
	}

	// The upload is archived, not physically deleted: hidden from current
	// reads, blob retained for the recovery window, attributed to the person.
	if _, err := mediaStore.Get(t.Context(), pictureID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("media still current after removal: %v", err)
	}
	archived, err := mediaStore.GetIncludingDeleted(t.Context(), pictureID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.DeletedAt == nil || archived.DeletedBy == nil || *archived.DeletedBy != id || archived.BlobPurgedAt != nil {
		t.Fatalf("archived %+v", archived)
	}
	if _, ok := blobs.Get(picture.Key); !ok {
		t.Fatal("blob deleted on removal")
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodDelete, "/v1/users/me/profile-picture", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("repeated delete status %d body %s", resp.StatusCode, b)
	}
	if again, err := mediaStore.GetIncludingDeleted(t.Context(), pictureID); err != nil || !again.DeletedAt.Equal(*archived.DeletedAt) {
		t.Fatalf("repeated delete touched the archive: %+v %v", again, err)
	}
}

func TestDeleteProfilePictureBlockedAccountHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.MustParse("99999999-9999-9999-9999-999999999994")
	profile := user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}
	mediaSvc := media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	app := meIdentApp(t, store, id, profile, mediaSvc)

	body, ctype := multipartPNG(t, "image", "dot.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/users/me/profile-picture", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status %d body %s", resp.StatusCode, b)
	}
	if _, err := store.RequestDeletion(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}

	// Through the normal chain the JIT guard already refuses the token.
	resp, err = app.Test(httptest.NewRequest(fiber.MethodDelete, "/v1/users/me/profile-picture", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("jit status %d body %s", resp.StatusCode, b)
	}

	// The handler is guarded too, in case the state changes after JIT: the
	// same blocked state yields the same 401, and nothing is archived.
	svc := user.NewService(store)
	me := NewMeHandler(svc, mediaSvc)
	direct := fiber.New()
	direct.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: id, Profile: profile})
		blocked, err := store.Get(c.Context(), id)
		if err != nil {
			return err
		}
		c.Locals(authn.LocalsUser, blocked)
		return c.Next()
	})
	direct.Delete("/v1/users/me/profile-picture", me.DeleteProfilePicture)
	resp, err = direct.Test(httptest.NewRequest(fiber.MethodDelete, "/v1/users/me/profile-picture", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("direct status %d body %s", resp.StatusCode, b)
	}
	if resp.Header.Get(fiber.HeaderCacheControl) != "no-store" || resp.Header.Get(fiber.HeaderWWWAuthenticate) != `Bearer error="invalid_token"` {
		t.Fatalf("blocked headers %v", resp.Header)
	}
	stored, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProfilePictureID == nil || stored.ProfilePictureURL == "" {
		t.Fatalf("blocked account lost its picture %+v", stored)
	}
	if _, err := mediaSvc.Get(t.Context(), *stored.ProfilePictureID); err != nil {
		t.Fatalf("blocked account's picture was archived: %v", err)
	}
}

// flakyUsers and flakyMedia wrap the two collaborators of
// DeleteProfilePicture so one step can fail exactly once, proving a retry
// converges whichever step failed.
type flakyUsers struct {
	user.Service
	failClear bool
}

type flakyMedia struct {
	media.Service
	failArchive bool
}

var errTransient = errors.New("transient store failure")

func (f *flakyUsers) ClearProfilePicture(ctx context.Context, id uuid.UUID) (*uuid.UUID, error) {
	if f.failClear {
		f.failClear = false
		return nil, errTransient
	}
	return f.Service.ClearProfilePicture(ctx, id)
}

func (f *flakyMedia) ArchiveOwn(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if f.failArchive {
		f.failArchive = false
		return errTransient
	}
	return f.Service.ArchiveOwn(ctx, p, id)
}

func uploadOwnPicture(t *testing.T, app *fiber.App) uuid.UUID {
	t.Helper()
	body, ctype := multipartPNG(t, "image", "dot.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/users/me/profile-picture", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status %d body %s", resp.StatusCode, b)
	}
	var uploaded user.User
	if err := json.NewDecoder(resp.Body).Decode(&uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.ProfilePictureID == nil {
		t.Fatalf("upload %+v", uploaded)
	}
	return *uploaded.ProfilePictureID
}

func TestDeleteProfilePictureConvergesAfterPartialFailureHTTP(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		failArchive bool
		failClear   bool
	}{
		{name: "archive fails first", failArchive: true},
		{name: "clear fails after archive", failClear: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := user.NewMemoryStore()
			id := uuid.New()
			mediaStore := media.NewMemoryStore()
			users := &flakyUsers{Service: user.NewService(store), failClear: tc.failClear}
			mediaSvc := &flakyMedia{
				Service:     media.NewService(mediaStore, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test"),
				failArchive: tc.failArchive,
			}
			jit := middlewares.NewJIT(users)
			me := NewMeHandler(users, mediaSvc)
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				c.Locals(authn.LocalsIdentity, authn.Identity{ID: id, Profile: user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}})
				return c.Next()
			})
			app.Post("/v1/users/me/profile-picture", jit.Handle, me.ProfilePicture)
			app.Delete("/v1/users/me/profile-picture", jit.Handle, me.DeleteProfilePicture)
			pictureID := uploadOwnPicture(t, app)

			resp, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/v1/users/me/profile-picture", nil))
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != fiber.StatusInternalServerError {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("failed removal status %d body %s", resp.StatusCode, b)
			}
			// Whatever the failed step, the picture is still linked, so the
			// retry can see it and finish the job.
			stored, err := store.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if stored.ProfilePictureID == nil || *stored.ProfilePictureID != pictureID {
				t.Fatalf("failed removal lost the link: %+v", stored)
			}

			resp, err = app.Test(httptest.NewRequest(fiber.MethodDelete, "/v1/users/me/profile-picture", nil))
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != fiber.StatusNoContent {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("retry status %d body %s", resp.StatusCode, b)
			}
			stored, err = store.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if stored.ProfilePictureID != nil || stored.ProfilePictureURL != "" {
				t.Fatalf("retry left the link: %+v", stored)
			}
			archived, err := mediaStore.GetIncludingDeleted(t.Context(), pictureID)
			if err != nil {
				t.Fatal(err)
			}
			if archived.DeletedAt == nil || archived.DeletedBy == nil || *archived.DeletedBy != id {
				t.Fatalf("retry left the upload live: %+v", archived)
			}
		})
	}
}

func TestProfilePictureUploadsAsProfilePictureHTTP(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("89898989-8989-8989-8989-898989898989")
	mediaSvc := media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	app := meIdentApp(t, user.NewMemoryStore(), id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}, mediaSvc)
	post := func(filename string, data []byte) *http.Response {
		t.Helper()
		body, ctype := multipartPNG(t, "image", filename, data)
		req := httptest.NewRequest(fiber.MethodPost, "/v1/users/me/profile-picture", body)
		req.Header.Set("Content-Type", ctype)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post("cv.pdf", []byte("%PDF-1.7\n"))
	var refused map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&refused); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnsupportedMediaType || refused["code"] != "media_type_not_allowed" || refused["purpose"] != "profile_picture" {
		t.Fatalf("PDF profile picture: status %d body %v", resp.StatusCode, refused)
	}

	resp = post("dot.png", pngDotHTTP())
	var got user.User
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != fiber.StatusOK || got.ProfilePictureID == nil {
		t.Fatalf("PNG profile picture: status %d user %+v err %v", resp.StatusCode, got, err)
	}
	picture, err := mediaSvc.Get(t.Context(), *got.ProfilePictureID)
	if err != nil || picture.Purpose != "profile_picture" {
		t.Fatalf("picture %+v err %v", picture, err)
	}
}
