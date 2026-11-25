package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
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

// GET /v1/certificate-assets/{id}/info and …/preview: an SVG is answered
// as itself, sandboxed and not sniffed, kept five minutes by the caller's
// browser only; a PDF without pdf-tools is a 503 problem with
// pdf_tools_unavailable; a bad max is 400; someone else's asset is 403.
func TestCertificateAssetsHTTP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mediaStore := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	store := certificate.NewMemoryStore()
	certs := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()),
		staticPDF("%PDF-1.4"), &recCertMail{}, certificate.Options{Templates: store, Artifacts: blobs, Assets: certificate.MediaAssets{Media: mediaStore, Blobs: blobs}})
	owner := authn.Identity{ID: uuid.New()}
	upload := func(name, contentType string, data []byte) uuid.UUID {
		t.Helper()
		if err := blobs.Put(ctx, "files/"+name, data, media.ServingMetadata(contentType, "")); err != nil {
			t.Fatal(err)
		}
		created, err := mediaStore.Create(ctx, media.Media{Name: name, Type: contentType, Kind: media.KindFile, Key: "files/" + name, UploadedBy: owner.ID, Purpose: media.PurposeLegacy})
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	svg := upload("logo.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100"/>`))
	pdf := upload("background.pdf", "application/pdf", []byte("%PDF-1.4 background"))
	call := func(who authn.Identity, path string) (int, fiber.Map, map[string]string, []byte) {
		t.Helper()
		ch := NewCertificateHandler(certs)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, who)
			return c.Next()
		})
		app.Get("/v1/certificate-assets/:id/info", ch.AssetInfo)
		app.Get("/v1/certificate-assets/:id/preview", ch.AssetPreview)
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		var out fiber.Map
		_ = json.Unmarshal(raw, &out)
		headers := map[string]string{}
		for _, name := range []string{fiber.HeaderContentType, fiber.HeaderCacheControl, fiber.HeaderXContentTypeOptions, fiber.HeaderContentSecurityPolicy} {
			headers[name] = resp.Header.Get(name)
		}
		return resp.StatusCode, out, headers, raw
	}

	status, info, headers, _ := call(owner, "/v1/certificate-assets/"+svg.String()+"/info")
	if status != fiber.StatusOK || info["widthPx"] != 200.0 || info["heightPx"] != 100.0 || headers[fiber.HeaderCacheControl] != "private, max-age=300" {
		t.Fatalf("svg info: %d %v %v", status, info, headers)
	}
	status, _, headers, body := call(owner, "/v1/certificate-assets/"+svg.String()+"/preview")
	if status != fiber.StatusOK || headers[fiber.HeaderContentType] != "image/svg+xml" || headers[fiber.HeaderContentSecurityPolicy] != "sandbox" ||
		headers[fiber.HeaderXContentTypeOptions] != "nosniff" || headers[fiber.HeaderCacheControl] != "private, max-age=300" || len(body) == 0 {
		t.Fatalf("svg preview: %d %v", status, headers)
	}
	status, problem, headers, _ := call(owner, "/v1/certificate-assets/"+pdf.String()+"/preview")
	if status != fiber.StatusServiceUnavailable || problem["code"] != certificate.CodePDFToolsUnavailable || headers[fiber.HeaderContentType] != "application/problem+json" {
		t.Fatalf("pdf without pdf-tools: %d %v %v", status, problem, headers)
	}
	if status, _, _, _ := call(owner, "/v1/certificate-assets/"+svg.String()+"/preview?max=abc"); status != fiber.StatusBadRequest {
		t.Fatalf("bad max: %d", status)
	}
	if status, _, _, _ := call(authn.Identity{ID: uuid.New()}, "/v1/certificate-assets/"+svg.String()+"/info"); status != fiber.StatusForbidden {
		t.Fatalf("someone else: %d", status)
	}
}
