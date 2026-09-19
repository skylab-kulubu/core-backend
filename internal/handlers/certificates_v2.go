package handlers

import (
	"net/url"
	"os"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
)

func (h *CertificateHandler) PublicPage(c fiber.Ctx) error {
	origin := strings.TrimRight(os.Getenv("CERTIFICATE_PUBLIC_PAGE_ORIGIN"), "/")
	if origin == "" {
		origin = "https://yildizskylab.com/sertifika"
	}
	c.Set(fiber.HeaderLocation, origin+"/"+url.PathEscape(c.Params("serial")))
	return c.SendStatus(fiber.StatusFound)
}

func (h *CertificateHandler) Summary(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.Summary(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) Finalize(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.Finalize(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(result)
}

func (h *CertificateHandler) Resolve(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.ResolveTemplate(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) PreviewEvent(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	pdf, err := h.svc.PreviewEvent(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderContentType, "application/pdf")
	return c.Send(pdf)
}

func (h *CertificateHandler) ListBatches(c fiber.Ctx) error {
	p, eventID, err := certificateEventCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.ListBatches(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) GetBatch(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.GetBatch(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) RetryBatch(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.RetryBatch(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(result)
}

func (h *CertificateHandler) Reissue(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.Reissue(c.Context(), p, c.Params("serial"))
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(result)
}

func (h *CertificateHandler) ListTemplates(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.ListTemplates(c.Context(), p)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) GetTemplate(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.GetTemplate(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) CreateTemplate(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	var input certificate.TemplateDraft
	if err := c.Bind().Body(&input); err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.CreateTemplate(c.Context(), p, input)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(result)
}

func (h *CertificateHandler) UpdateTemplate(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	var input certificate.TemplateDraft
	if err := c.Bind().Body(&input); err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.UpdateTemplate(c.Context(), p, id, input)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) PublishTemplate(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.PublishTemplate(c.Context(), p, id)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(result)
}

func (h *CertificateHandler) PreviewTemplate(c fiber.Ctx) error {
	p, id, err := certificateTemplateCaller(c)
	if err != nil {
		return certError(c, err)
	}
	var sample certificate.PreviewData
	if c.Request().Header.ContentLength() > 0 {
		if err := c.Bind().Body(&sample); err != nil {
			return certError(c, certificate.ErrInvalid)
		}
	}
	pdf, err := h.svc.PreviewTemplate(c.Context(), p, id, sample)
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderContentType, "application/pdf")
	return c.Send(pdf)
}

func (h *CertificateHandler) SetBinding(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	var input certificate.Binding
	if err := c.Bind().Body(&input); err != nil {
		return certError(c, certificate.ErrInvalid)
	}
	result, err := h.svc.SetBinding(c.Context(), p, input)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) ListBindings(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	result, err := h.svc.ListBindings(c.Context(), p)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(result)
}

func (h *CertificateHandler) ClearBinding(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	if err := h.svc.ClearBinding(c.Context(), p, c.Params("scope"), c.Params("scopeKey")); err != nil {
		return certError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func certificateEventCaller(c fiber.Ctx) (authz.Principal, uuid.UUID, error) {
	p, err := caller(c)
	if err != nil {
		return authz.Principal{}, uuid.Nil, err
	}
	id, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return authz.Principal{}, uuid.Nil, certificate.ErrInvalid
	}
	return p, id, nil
}

func certificateTemplateCaller(c fiber.Ctx) (authz.Principal, uuid.UUID, error) {
	p, err := caller(c)
	if err != nil {
		return authz.Principal{}, uuid.Nil, err
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return authz.Principal{}, uuid.Nil, certificate.ErrInvalid
	}
	return p, id, nil
}
