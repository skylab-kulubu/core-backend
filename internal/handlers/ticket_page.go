package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
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

// TicketQRsPerClientMinute is the QR image's budget a minute per client
// address, its own: the mail shows it as a remote image, and Gmail's image
// proxy and Apple Mail's privacy preloading fetch it for many people from a
// few addresses (a published Event's mails go out together). The PNG is
// cheap to draw.
const TicketQRsPerClientMinute = 1200

// TicketCheckInsPerKeyMinute is how often one ticket's page may ask for
// its check-ins in a minute: the page asks every 4 seconds while it is
// shown on the Event's day (15 a minute), so two open tabs fit.
const TicketCheckInsPerKeyMinute = 30

// TicketCheckInsPerClientMinute is the check-ins route's budget a minute
// per client address, its own: at the door many holders share one address
// (the campus network, a mobile operator's NAT), each polling at 15 a
// minute. The per-key budget is the one that holds a page back.
const TicketCheckInsPerClientMinute = 3000

// TicketCheckInsLimit budgets the check-ins route by the key in its path
// (a hash of it, as TicketPDFLimit).
func TicketCheckInsLimit() fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        TicketCheckInsPerKeyMinute,
		Expiration: time.Minute,
		KeyGenerator: func(c fiber.Ctx) string {
			sum := sha256.Sum256([]byte(c.Params("key")))
			return "ticket-check-ins:" + hex.EncodeToString(sum[:12])
		},
		LimitReached: func(c fiber.Ctx) error {
			ticketPageHeaders(c)
			c.Set(fiber.HeaderRetryAfter, "60")
			return problemDetailCode(c, fiber.StatusTooManyRequests, "Too Many Requests",
				"Too many requests for this ticket; retry in a minute.", "ticket_check_ins_rate_limited")
		},
	})
}

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
	case errors.Is(err, ticket.ErrCancelClosed):
		return problemCode(c, fiber.StatusConflict, "Conflict", "ticket_cancel_closed")
	case errors.Is(err, ticket.ErrTicketCheckedIn):
		return problemCode(c, fiber.StatusConflict, "Conflict", "ticket_checked_in")
	case errors.Is(err, ticket.ErrTicketInCompetition):
		return problemCode(c, fiber.StatusConflict, "Conflict", "ticket_in_competition")
	case errors.Is(err, ticket.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, ticketpage.ErrUnavailable):
		if !errors.Is(err, context.Canceled) {
			h.logger.Printf("ticket page: %s %s: %v", route, ticketpage.LogKey(c.Params("key")), err)
		}
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

// TicketCancelsPerClientMinute is the cancel route's budget a minute per
// client address (kontenjan-iptal-hatirlatma 03).
const TicketCancelsPerClientMinute = 10

// Cancel is POST /v1/public/tickets/{key}/cancel: no body; 200
// {"state":"ticket_cancelled"}, again for a Ticket already cancelled. The
// log line names the request alone: never the key, the Ticket or a person.
func (h *TicketPageHandler) Cancel(c fiber.Ctx) error {
	ticketPageHeaders(c)
	view, err := h.svc.Cancel(c.Context(), c.Params("key"))
	if err != nil {
		return h.ticketPageError(c, "cancel", err)
	}
	h.logger.Printf(`{"event":"ticket_cancelled_by_holder","route":"ticket_page","correlation_id":%q}`, requestid.FromContext(c))
	return c.JSON(view)
}

// CheckIns is GET /v1/public/tickets/{key}/check-ins.
func (h *TicketPageHandler) CheckIns(c fiber.Ctx) error {
	ticketPageHeaders(c)
	view, err := h.svc.CheckIns(c.Context(), c.Params("key"))
	if err != nil {
		return h.ticketPageError(c, "check-ins", err)
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
