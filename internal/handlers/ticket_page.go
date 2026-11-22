package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
)

// TicketPageHandler answers the ticket page's public routes
// (docs/ticket-page.md): no sign-in, the access key in the path is the
// right to read.
type TicketPageHandler struct {
	svc    *ticketpage.Service
	logger *log.Logger
}

func NewTicketPageHandler(svc *ticketpage.Service) *TicketPageHandler {
	return &TicketPageHandler{svc: svc, logger: log.Default()}
}

// TicketPDFsPerKeyMinute is how many PDFs one ticket's key may ask for in a
// minute: each one is drawn anew.
const TicketPDFsPerKeyMinute = 5

// TicketPDFLimit budgets the PDF route by the key in its path (a hash of
// it: the limiter's memory never holds the key).
func TicketPDFLimit() fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        TicketPDFsPerKeyMinute,
		Expiration: time.Minute,
		KeyGenerator: func(c fiber.Ctx) string {
			sum := sha256.Sum256([]byte(c.Params("key")))
			return "ticket-pdf:" + hex.EncodeToString(sum[:12])
		},
		LimitReached: func(c fiber.Ctx) error {
			ticketPageHeaders(c)
			return problemDetailCode(c, fiber.StatusTooManyRequests, "Too Many Requests",
				"Too many PDFs for this ticket; retry in a minute.", "ticket_pdf_rate_limited")
		},
	})
}

// ticketPageHeaders keep the key out of caches, referrers and indexes.
func ticketPageHeaders(c fiber.Ctx) {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderReferrerPolicy, "no-referrer")
	c.Set("X-Robots-Tag", "noindex, nofollow")
}

// ticketPageError answers a refusal with no personal data. An unexpected
// error is logged here, naming the key by its first four characters only,
// so the error handler never writes the whole path.
func (h *TicketPageHandler) ticketPageError(c fiber.Ctx, route string, err error) error {
	switch {
	case errors.Is(err, ticketpage.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, ticketpage.ErrGone):
		return problemCode(c, fiber.StatusGone, "Gone", "ticket_page_closed")
	case errors.Is(err, ticketpage.ErrUnavailable):
		c.Set(fiber.HeaderRetryAfter, "30")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "ticket_pdf_unavailable")
	default:
		h.logger.Printf("ticket page: %s %s: %v", route, ticketpage.LogKey(c.Params("key")), err)
		return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
	}
}

// Page is GET /v1/public/tickets/{key}.
func (h *TicketPageHandler) Page(c fiber.Ctx) error {
	ticketPageHeaders(c)
	view, err := h.svc.Page(c.Context(), c.Params("key"))
	if err != nil {
		return h.ticketPageError(c, "page", err)
	}
	return c.JSON(view)
}

// QR is GET /v1/public/tickets/{key}/qr.png.
func (h *TicketPageHandler) QR(c fiber.Ctx) error {
	ticketPageHeaders(c)
	png, err := h.svc.QRPNG(c.Context(), c.Params("key"))
	if err != nil {
		return h.ticketPageError(c, "qr", err)
	}
	c.Set(fiber.HeaderContentType, "image/png")
	return c.Send(png)
}

// PDF is GET /v1/public/tickets/{key}/pdf.
func (h *TicketPageHandler) PDF(c fiber.Ctx) error {
	ticketPageHeaders(c)
	name, pdf, err := h.svc.PDF(c.Context(), c.Params("key"))
	if err != nil {
		return h.ticketPageError(c, "pdf", err)
	}
	c.Set(fiber.HeaderContentType, "application/pdf")
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+name+`"`)
	c.Set(fiber.HeaderContentLength, strconv.Itoa(len(pdf)))
	return c.Send(pdf)
}
