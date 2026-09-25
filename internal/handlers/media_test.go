package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func pngDotHTTP() []byte {
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	return b
}

func mediaApp(t *testing.T, ident authn.Identity, store media.Store, blobs media.BlobStore) *fiber.App {
	t.Helper()
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	h := NewMediaHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Post("/v1/media", h.Upload)
	app.Get("/v1/media", h.List)
	app.Get("/v1/media/:id", h.Get)
	app.Delete("/v1/media/:id", h.Delete)
	app.Post("/v1/media/:id/restore", h.Restore)
	return app
}

func multipartPNG(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func TestMediaUploadAndPublicGetHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ident := authn.Identity{ID: userID, Profile: user.Profile{Email: "ada@example.com"}}
	app := mediaApp(t, ident, store, blobs)

	body, ctype := multipartPNG(t, "file", "dot.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status %d body %s", resp.StatusCode, b)
	}
	var created media.Media
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.UploadedBy != userID || created.Kind != media.KindImage {
		t.Fatalf("created %+v", created)
	}

	public := mediaApp(t, authn.Identity{}, store, blobs)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/media/"+created.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	var got media.Media
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.URL != created.URL {
		t.Fatalf("got %+v", got)
	}
}

func TestMediaListAndPrivilegedDeleteHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	member := authn.Identity{ID: userID, Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app := mediaApp(t, member, store, blobs)

	body, ctype := multipartPNG(t, "file", "dot.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status %d body %s", resp.StatusCode, b)
	}
	var created media.Media
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/media", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("member list %d", resp.StatusCode)
	}

	ykList := authn.Identity{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), Groups: []string{"/UYELER/YK"}}
	listApp := mediaApp(t, ykList, store, blobs)
	resp, err = listApp.Test(httptest.NewRequest(fiber.MethodGet, "/v1/media", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("privileged list status %d", resp.StatusCode)
	}
	var listed []media.Media
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed %+v", listed)
	}

	delPath := "/v1/media/" + created.ID.String()
	resp, err = app.Test(httptest.NewRequest(fiber.MethodDelete, delPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("member delete %d body %s", resp.StatusCode, b)
	}

	yk := authn.Identity{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), Groups: []string{"/UYELER/YK"}}
	app = mediaApp(t, yk, store, blobs)
	resp, err = app.Test(httptest.NewRequest(fiber.MethodDelete, delPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("yk delete %d body %s", resp.StatusCode, b)
	}
}

func TestMediaAnonymousUploadForbiddenHTTP(t *testing.T) {
	t.Parallel()
	app := mediaApp(t, authn.Identity{}, media.NewMemoryStore(), media.NewMemoryBlob())
	body, ctype := multipartPNG(t, "file", "dot.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
}

func TestMediaLifecycleHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	uploaderID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	created, err := store.Create(t.Context(), media.Media{
		Name: "guide.pdf", Type: "application/pdf", Size: 42, UploadedBy: uploaderID,
		Kind: media.KindFile, Key: "files/guide",
	})
	if err != nil {
		t.Fatal(err)
	}
	managerID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	manager := mediaApp(t, authn.Identity{ID: managerID, Groups: []string{"/UYELER/YK"}}, store, blobs)
	path := "/v1/media/" + created.ID.String()

	requireStatus(t, manager, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, manager, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, mediaApp(t, authn.Identity{}, store, blobs), fiber.MethodGet, path, fiber.StatusNotFound)
	requireStatus(t, manager, fiber.MethodGet, "/v1/media?lifecycle=invalid", fiber.StatusBadRequest)

	resp, err := manager.Test(httptest.NewRequest(fiber.MethodGet, "/v1/media", nil))
	if err != nil {
		t.Fatal(err)
	}
	var current []media.Media
	if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("current media = %+v", current)
	}

	resp, err = manager.Test(httptest.NewRequest(fiber.MethodGet, "/v1/media?lifecycle=inactive", nil))
	if err != nil {
		t.Fatal(err)
	}
	var archived []media.Media
	if err := json.NewDecoder(resp.Body).Decode(&archived); err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ID != created.ID || archived[0].DeletedAt == nil || archived[0].DeletedBy == nil || *archived[0].DeletedBy != managerID {
		t.Fatalf("archived media = %+v", archived)
	}
	requireStatus(t, manager, fiber.MethodGet, "/v1/media?lifecycle=all", fiber.StatusOK)
	member := mediaApp(t, authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}, store, blobs)
	requireStatus(t, member, fiber.MethodGet, "/v1/media?lifecycle=inactive", fiber.StatusForbidden)

	restorePath := path + "/restore"
	requireStatus(t, manager, fiber.MethodPost, restorePath, fiber.StatusOK)
	requireStatus(t, manager, fiber.MethodPost, restorePath, fiber.StatusOK)
	requireStatus(t, manager, fiber.MethodGet, path, fiber.StatusOK)

	requireStatus(t, manager, fiber.MethodDelete, path, fiber.StatusNoContent)
	wantPurgeErr := errors.New("storage unavailable")
	if _, err := store.PurgeBlobIfUnreferenced(t.Context(), created.ID, time.Now().UTC(), func(string) error { return wantPurgeErr }); !errors.Is(err, wantPurgeErr) {
		t.Fatalf("purge error = %v", err)
	}
	requireStatus(t, manager, fiber.MethodPost, restorePath, fiber.StatusConflict)
	purged, err := store.PurgeBlobIfUnreferenced(t.Context(), created.ID, time.Now().UTC(), func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !purged {
		t.Fatal("expected archived blob to be purged")
	}
	requireStatus(t, manager, fiber.MethodPost, restorePath, fiber.StatusGone)

	pending, err := store.Create(t.Context(), media.Media{
		Name: "pending.pdf", Type: "application/pdf", Size: 24, UploadedBy: uploaderID,
		Kind: media.KindFile, Key: "files/pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	pendingPath := "/v1/media/" + pending.ID.String()
	requireStatus(t, manager, fiber.MethodDelete, pendingPath, fiber.StatusNoContent)
	claimErr := errors.New("storage temporarily unavailable")
	if _, err := store.PurgeBlobIfUnreferenced(t.Context(), pending.ID, time.Now().UTC(), func(string) error { return claimErr }); !errors.Is(err, claimErr) {
		t.Fatalf("purge claim error = %v", err)
	}
	requireStatus(t, manager, fiber.MethodPost, pendingPath+"/restore", fiber.StatusConflict)
}

func uploadPNGHTTP(t *testing.T, app *fiber.App, filename string) media.Media {
	t.Helper()
	body, ctype := multipartPNG(t, "file", filename, pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status %d body %s", resp.StatusCode, b)
	}
	var created media.Media
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return created
}

func getMediaJSON(t *testing.T, app *fiber.App, id uuid.UUID) map[string]any {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/media/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMediaGetHidesUploaderAndNameFromAnonymousCallersHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	uploader := authn.Identity{ID: uuid.MustParse("12121212-1212-1212-1212-121212121212")}
	created := uploadPNGHTTP(t, mediaApp(t, uploader, store, blobs), "Ayşe Yılmaz - CV.png")

	got := getMediaJSON(t, mediaApp(t, authn.Identity{}, store, blobs), created.ID)
	for _, hidden := range []string{"uploadedBy", "deletedBy", "name"} {
		if _, ok := got[hidden]; ok {
			t.Errorf("anonymous response carries %q: %v", hidden, got)
		}
	}
	if got["id"] != created.ID.String() || got["url"] != created.URL || got["type"] != "image/png" {
		t.Fatalf("anonymous response %v", got)
	}
}

func TestMediaGetKeepsUploaderAndNameForSignedInCallersHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	uploader := authn.Identity{ID: uuid.MustParse("13131313-1313-1313-1313-131313131313")}
	created := uploadPNGHTTP(t, mediaApp(t, uploader, store, blobs), "Ayşe Yılmaz - CV.png")

	reviewer := authn.Identity{ID: uuid.MustParse("14141414-1414-1414-1414-141414141414")}
	got := getMediaJSON(t, mediaApp(t, reviewer, store, blobs), created.ID)
	if got["uploadedBy"] != uploader.ID.String() || got["name"] != "Ayşe Yılmaz - CV.png" {
		t.Fatalf("signed-in response %v", got)
	}
}
