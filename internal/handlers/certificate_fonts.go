package handlers

import (
	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

// fontCacheControl lets the editor's own browser keep a certificate font's
// file a day: the file of a font never changes (a new upload is a new
// font), and only who may use it reads it.
const fontCacheControl = "private, max-age=86400"

// ListFonts answers GET /v1/certificate-fonts?team={code}: the current
// certificate fonts of the team and the club the caller may use
// (docs/certificate-fonts.md); without team, the club's.
func (h *CertificateHandler) ListFonts(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	fonts, err := h.svc.ListFonts(c.Context(), p, c.Query("team"))
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(fonts)
}

// CreateFont answers POST /v1/certificate-fonts {mediaId, ownerTeam}: the
// caller's certificate_font upload, checked by pdf-tools, kept as the
// team's (or the club's) font. 201 with the font.
func (h *CertificateHandler) CreateFont(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	var input certificate.FontInput
	if err := c.Bind().Body(&input); err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	font, err := h.svc.CreateFont(c.Context(), p, input)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(font)
}

// FontFile answers GET /v1/certificate-fonts/{id}/file: the font's checked
// WOFF2, which the editor loads with the FontFace API.
func (h *CertificateHandler) FontFile(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	file, err := h.svc.FontFile(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderContentType, "font/woff2")
	c.Set(fiber.HeaderCacheControl, fontCacheControl)
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	return c.Send(file.Data)
}

// ArchiveFont answers DELETE /v1/certificate-fonts/{id}: the font is
// archived, not deleted (204). Drafts and versions that use it go on
// printing it; no new element may.
func (h *CertificateHandler) ArchiveFont(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	if err := h.svc.ArchiveFont(c.Context(), p, id); err != nil {
		return certError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
