package handlers

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

// assetCacheControl lets the editor's own browser keep an asset's info and
// preview for five minutes: an asset does not change, and only who may
// edit it reads it.
const assetCacheControl = "private, max-age=300"

// maxTextConversionBody bounds a text conversion's request: 500 ids and
// their kinds fit many times over.
const maxTextConversionBody = 256 << 10

// AssetInfo answers GET /v1/certificate-assets/{id}/info: the asset's size
// in layout px, with a PDF's page and text counts (pdf-tools).
func (h *CertificateHandler) AssetInfo(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	info, err := h.svc.AssetInfo(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, assetCacheControl)
	return c.JSON(info)
}

// AssetPreview answers GET /v1/certificate-assets/{id}/preview?max=2000: a
// PDF's page as a PNG of that long side (pdf-tools), an image as itself.
// An SVG is answered sandboxed, so it runs nothing opened on its own.
func (h *CertificateHandler) AssetPreview(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	maxPx := certificate.DefaultAssetPreviewPx
	if raw := strings.TrimSpace(c.Query("max")); raw != "" {
		if maxPx, err = strconv.Atoi(raw); err != nil {
			return certError(c, certificate.ErrInvalid)
		}
	}
	preview, err := h.svc.AssetPreview(c.Context(), p, id, maxPx)
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderContentType, preview.ContentType)
	c.Set(fiber.HeaderCacheControl, assetCacheControl)
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	if preview.ContentType == "image/svg+xml" {
		c.Set(fiber.HeaderContentSecurityPolicy, "sandbox")
	}
	return c.Send(preview.Data)
}

// AssetText answers GET /v1/certificate-assets/{id}/text: a PDF asset's
// text, piece by piece, with the font each would be set in (pdf-tools).
func (h *CertificateHandler) AssetText(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	text, err := h.svc.AssetText(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, assetCacheControl)
	return c.JSON(text)
}

// ConvertAssetText answers POST /v1/certificate-assets/{id}/text-conversion:
// the chosen pieces of a PDF background's text as layout elements, over a
// new PDF without them, which the editor applies to its draft.
func (h *CertificateHandler) ConvertAssetText(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	if len(c.Body()) > maxTextConversionBody {
		return problem(c, fiber.StatusRequestEntityTooLarge, "Request Entity Too Large")
	}
	var req certificate.TextConversionRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	conversion, err := h.svc.ConvertAssetText(c.Context(), p, id, req)
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(conversion)
}
