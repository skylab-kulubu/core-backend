package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"testing"

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
	app.Get("/v1/media/:id", h.Get)
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
