package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/qr"
)

type CertificateHandler struct {
	svc certificate.Service
}

func NewCertificateHandler(svc certificate.Service) *CertificateHandler {
	return &CertificateHandler{svc: svc}
}

func certError(c fiber.Ctx, err error) error {
	if handled, problemErr := linkProblem(c, err); handled {
		return problemErr
	}
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, certificate.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, certificate.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, certificate.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, certificate.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (h *CertificateHandler) Verify(c fiber.Ctx) error {
	got, err := h.svc.Verify(c.Context(), c.Params("serial"))
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(got)
}

func (h *CertificateHandler) Download(c fiber.Ctx) error {
	pdf, err := h.svc.PDF(c.Context(), c.Params("serial"))
	if err != nil {
		return certError(c, err)
	}
	c.Set(fiber.HeaderContentType, "application/pdf")
	return c.Send(pdf)
}

func (h *CertificateHandler) QR(c fiber.Ctx) error {
	got, err := h.svc.Verify(c.Context(), c.Params("serial"))
	if err != nil {
		return certError(c, err)
	}
	png, err := qr.PNG(got.VerifyURL, qr.SizeFromQuery(c.Query("size")))
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, "image/png")
	return c.Send(png)
}

func (h *CertificateHandler) Mine(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	listed, err := h.svc.Mine(c.Context(), p)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(listed)
}

func (h *CertificateHandler) ListByEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	listed, err := h.svc.ListByEvent(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.JSON(listed)
}

type issueBody struct {
	TicketID uuid.UUID `json:"ticketId"`
}

func (h *CertificateHandler) Issue(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body issueBody
	if err := c.Bind().Body(&body); err != nil || body.TicketID == uuid.Nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.QueueManual(c.Context(), p, eventID, body.TicketID)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(created)
}

func (h *CertificateHandler) Recompute(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	issued, err := h.svc.Finalize(c.Context(), p, eventID)
	if err != nil {
		return certError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(issued)
}

func (h *CertificateHandler) Revoke(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return certError(c, err)
	}
	if err := h.svc.Revoke(c.Context(), p, c.Params("serial")); err != nil {
		return certError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
