package handlers

import (
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
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// GET /v1/certificate-assets/{id}/text and POST …/text-conversion: a
// malformed body is a bare 400, a PDF without pdf-tools a 503 problem with
// pdf_tools_unavailable, an empty choice background_text_selection_empty.
func TestCertificateAssetTextHTTP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mediaStore := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	store := certificate.NewMemoryStore()
	certs := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()),
		staticPDF("%PDF-1.4"), &recCertMail{}, certificate.Options{Templates: store, Artifacts: blobs, Assets: certificate.MediaAssets{Media: mediaStore, Blobs: blobs}})
	owner := authn.Identity{ID: uuid.New()}
	if err := blobs.Put(ctx, "files/background.pdf", []byte("%PDF-1.4 background"), media.ServingMetadata("application/pdf", "")); err != nil {
		t.Fatal(err)
	}
	pdf, err := mediaStore.Create(ctx, media.Media{Name: "background.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/background.pdf", UploadedBy: owner.ID, Purpose: media.PurposeLegacy})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) (int, fiber.Map) {
		t.Helper()
		ch := NewCertificateHandler(certs)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, owner)
			return c.Next()
		})
		app.Get("/v1/certificate-assets/:id/text", ch.AssetText)
		app.Post("/v1/certificate-assets/:id/text-conversion", ch.ConvertAssetText)
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		var out fiber.Map
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	base := "/v1/certificate-assets/" + pdf.ID.String()
	if status, problem := call(fiber.MethodGet, base+"/text", ""); status != fiber.StatusServiceUnavailable || problem["code"] != certificate.CodePDFToolsUnavailable {
		t.Fatalf("text without pdf-tools: %d %v", status, problem)
	}
	if status, problem := call(fiber.MethodPost, base+"/text-conversion", "{"); status != fiber.StatusBadRequest || problem["code"] != nil {
		t.Fatalf("malformed body: %d %v", status, problem)
	}
	if status, problem := call(fiber.MethodPost, base+"/text-conversion", `{"spanIds":[],"page":{"width":1123,"height":794}}`); status != fiber.StatusBadRequest ||
		problem["code"] != certificate.CodeBackgroundTextSelectionEmpty {
		t.Fatalf("nothing chosen: %d %v", status, problem)
	}
	if status, problem := call(fiber.MethodPost, base+"/text-conversion", `{"spanIds":["3"],"page":{"width":1123,"height":794}}`); status != fiber.StatusBadRequest ||
		problem["code"] != certificate.CodeAssetStorageUnavailable {
		t.Fatalf("no storage for the new PDF: %d %v", status, problem)
	}
}
