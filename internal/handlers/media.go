package handlers

import (
	"errors"
	"io"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

type MediaHandler struct {
	svc media.Service
}

func NewMediaHandler(svc media.Service) *MediaHandler {
	return &MediaHandler{svc: svc}
}

// purposeProblem answers an upload its Media purpose refused: problem+json
// with a stable code and what the caller needs to fix the upload. handled is
// false for any other error.
func purposeProblem(c fiber.Ctx, err error) (handled bool, _ error) {
	var refusal *media.PurposeRefusal
	if !errors.As(err, &refusal) {
		return false, nil
	}
	fields := fiber.Map{"purpose": refusal.Purpose}
	switch {
	case errors.Is(err, media.ErrPurposeUnknown):
		return true, problemWithFields(c, fiber.StatusBadRequest, "Bad Request",
			"The purpose is not in the Media purpose catalogue.", "purpose_unknown", fields)
	case errors.Is(err, media.ErrTypeNotAllowed):
		fields["allowedTypes"] = refusal.AllowedTypes
		return true, problemWithFields(c, fiber.StatusUnsupportedMediaType, "Unsupported Media Type",
			"The file's content is not a type this purpose accepts.", "media_type_not_allowed", fields)
	case errors.Is(err, media.ErrTooLarge):
		fields["maxBytes"] = refusal.MaxBytes
		return true, problemWithFields(c, fiber.StatusRequestEntityTooLarge, "Content Too Large",
			"The file is larger than this purpose allows.", "media_too_large", fields)
	case errors.Is(err, media.ErrPurposeForbidden):
		return true, problemWithFields(c, fiber.StatusForbidden, "Forbidden",
			"The caller may not upload Media for this purpose.", "purpose_forbidden", fields)
	case errors.Is(err, media.ErrPrivateMediaDisabled):
		// Not 503: nothing here is transient, and 503 is kept for an
		// unreachable key service once private Media ships.
		return true, problemWithFields(c, fiber.StatusUnprocessableEntity, "Unprocessable Content",
			"Private Media is not available yet; this purpose cannot be uploaded.", "private_media_disabled", fields)
	case errors.Is(err, media.ErrPurposeNotAvailable):
		// Like private_media_disabled: nothing is stored, and retrying does
		// not help until a product attaches these Media.
		return true, problemWithFields(c, fiber.StatusUnprocessableEntity, "Unprocessable Content",
			"No product attaches Media of this purpose yet, so nothing could attach the file before it expires.",
			"purpose_not_available", fields)
	case errors.Is(err, media.ErrDirectUploadOnly):
		return true, problemWithFields(c, fiber.StatusBadRequest, "Bad Request",
			"This purpose is uploaded by Direct upload, not through this endpoint.", "purpose_requires_direct_upload", fields)
	default:
		return false, nil
	}
}

func mediaError(c fiber.Ctx, err error) error {
	if handled, problemErr := purposeProblem(c, err); handled {
		return problemErr
	}
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, media.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, media.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, media.ErrPurged):
		return problem(c, fiber.StatusGone, "Gone")
	case errors.Is(err, media.ErrPurgeInProgress):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, media.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (h *MediaHandler) Upload(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	header, err := c.FormFile("file")
	if err != nil || header == nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	f, err := header.Open()
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	var created media.Media
	if purpose := formPurpose(c); purpose != "" {
		created, err = h.svc.UploadForPurpose(c.Context(), p, purpose, media.UploadedFile{
			Name: header.Filename, ContentType: header.Header.Get("Content-Type"), Data: data,
		})
	} else {
		// Without a purpose the Media is legacy, under the rules Media
		// uploaded without a purpose have always had.
		created, err = h.svc.Upload(c.Context(), p, header.Filename, header.Header.Get("Content-Type"), data)
	}
	if err != nil {
		return mediaError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

// formPurpose is the upload's purpose field. It is read from the multipart
// body only: a purpose in the query string is ignored.
func formPurpose(c fiber.Ctx) string {
	form, err := c.MultipartForm()
	if err != nil || form == nil || len(form.Value["purpose"]) == 0 {
		return ""
	}
	return form.Value["purpose"][0]
}

func (h *MediaHandler) Get(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.Get(c.Context(), id)
	if err != nil {
		return mediaError(c, err)
	}
	if _, callerErr := caller(c); callerErr != nil {
		return c.JSON(publicMediaView(got))
	}
	return c.JSON(got)
}

// publicMedia is what a caller without a token sees of a media record:
// enough to render it, nothing about who uploaded it or what they named it.
// Until Media purpose ships, Answer files are still Media on this route.
type publicMedia struct {
	ID          uuid.UUID `json:"id"`
	Type        string    `json:"type"`
	URL         string    `json:"url"`
	Size        int64     `json:"size"`
	Kind        string    `json:"kind"`
	CoverColors []string  `json:"coverColors"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func publicMediaView(m media.Media) publicMedia {
	return publicMedia{
		ID:          m.ID,
		Type:        m.Type,
		URL:         m.URL,
		Size:        m.Size,
		Kind:        m.Kind,
		CoverColors: m.CoverColors,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

func (h *MediaHandler) List(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	visibility, err := lifecycleVisibility(c)
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var items []media.Media
	if visibility == lifecycle.CurrentOnly {
		items, err = h.svc.List(c.Context(), p)
	} else {
		items, err = h.svc.ListLifecycle(c.Context(), p, visibility)
	}
	if err != nil {
		return mediaError(c, err)
	}
	return c.JSON(items)
}

func (h *MediaHandler) Restore(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	restored, err := h.svc.Restore(c.Context(), p, id)
	if err != nil {
		return mediaError(c, err)
	}
	return c.JSON(restored)
}

func (h *MediaHandler) Delete(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.Delete(c.Context(), p, id); err != nil {
		return mediaError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
