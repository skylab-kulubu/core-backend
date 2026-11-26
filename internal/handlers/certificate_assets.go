package handlers

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

// assetCacheControl lets the editor's own browser keep an asset's info and
// preview for five minutes: an asset does not change, and only who may
// edit it reads it.
const assetCacheControl = "private, max-age=300"

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
