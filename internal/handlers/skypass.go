package handlers

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
)

type SkyPassHandler struct {
	svc     skypass.Service
	tickets ticket.Service
	// ticketKeys are the Ticket QR public keys published next to SkyPass's
	// (docs/ticket-qr.md); nil publishes none.
	ticketKeys TicketQRKeys
}

// TicketQRKeys are the Ticket QR public keys (*ticketqr.Signer).
type TicketQRKeys interface {
	JWKs() []ticketqr.JWK
}

func NewSkyPassHandler(svc skypass.Service, tickets ticket.Service) *SkyPassHandler {
	return &SkyPassHandler{svc: svc, tickets: tickets}
}

// WithTicketQRKeys publishes keys in the JWKS too.
func (h *SkyPassHandler) WithTicketQRKeys(keys TicketQRKeys) *SkyPassHandler {
	h.ticketKeys = keys
	return h
}

type cardBindBody struct {
	UID    string     `json:"uid"`
	UserID *uuid.UUID `json:"userId"`
}

type verifyBody struct {
	Token string `json:"token"`
}

type settleBody struct {
	Token string `json:"token"`
	UID   string `json:"uid"`
	// ScannedAt is when the door device took the scan, for a scan it
	// sends later (RFC 3339; docs/offline-door.md). Empty: now.
	ScannedAt string `json:"scannedAt"`
}

// scanContext is ctx carrying the body's scannedAt, checked against core's
// clock: 400 scanned_at_invalid for one that is not RFC 3339, 422
// scanned_at_in_future or scanned_at_too_old outside the bounds. ok false
// means the answer is written.
func scanContext(c fiber.Ctx, body settleBody) (context.Context, bool, error) {
	ctx := c.Context()
	raw := strings.TrimSpace(body.ScannedAt)
	if raw == "" {
		return ctx, true, nil
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return ctx, false, problemDetailCode(c, fiber.StatusBadRequest, "Bad Request",
			"scannedAt is not an RFC 3339 time.", "scanned_at_invalid")
	}
	switch err := ticket.CheckScannedAt(at, time.Now()); {
	case errors.Is(err, ticket.ErrScannedAtFuture):
		return ctx, false, problemDetailCode(c, fiber.StatusUnprocessableEntity, "Unprocessable Entity",
			"scannedAt is ahead of the server's clock; check the device's time.", "scanned_at_in_future")
	case errors.Is(err, ticket.ErrScannedAtTooOld):
		return ctx, false, problemDetailCode(c, fiber.StatusUnprocessableEntity, "Unprocessable Entity",
			"scannedAt is older than the door accepts (72 hours).", "scanned_at_too_old")
	}
	return skypass.WithScannedAt(ticket.WithScannedAt(ctx, at), at), true, nil
}

// offlineDuplicate answers a scan sent later whose Ticket is already in
// (that day's Venue entry, or that Session's Check-in): 200 with
// duplicate true and the record that is there (its id, scannedAt: null for
// a scan core took as it came, createdAt), so the door app sending its
// queue again, or two doors' queues, end as one record
// (docs/offline-door.md). handled false for anything else; a scan core took
// as it came never gets here (409 as before).
func offlineDuplicate(c fiber.Ctx, err error) (bool, error) {
	var dup *ticket.DuplicateScanError
	if !errors.As(err, &dup) {
		return false, nil
	}
	body := fiber.Map{"duplicate": true, "kind": dup.Kind, "scannedAt": dup.ScannedAt}
	if dup.ID != uuid.Nil {
		body["id"], body["createdAt"] = dup.ID, dup.CreatedAt
	}
	if dup.Kind == ticket.KindEntry {
		body["name"] = dup.Name
	}
	return true, c.Status(fiber.StatusOK).JSON(body)
}

func skypassError(c fiber.Ctx, err error) error {
	var limited *skypass.WalletRateLimitError
	switch {
	case errors.As(err, &limited):
		seconds := int(math.Ceil(limited.RetryAfter.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(seconds))
		return problemWithFields(c, fiber.StatusTooManyRequests, "Too Many Requests",
			"Too many wrong codes for this pass; retry after the given seconds.", "skypass_wallet_rate_limited",
			fiber.Map{"retryAfterSeconds": seconds})
	case errors.Is(err, skypass.ErrWalletCodeUsed):
		return problemDetailCode(c, fiber.StatusConflict, "Conflict",
			"This Wallet code was already used; the pass shows a new one within a minute.", "skypass_wallet_code_used")
	case errors.Is(err, skypass.ErrWalletOff):
		return problemDetailCode(c, fiber.StatusServiceUnavailable, "Service Unavailable",
			"Google Wallet is not set up.", "skypass_google_wallet_off")
	case errors.Is(err, skypass.ErrWalletUpstream):
		c.Set(fiber.HeaderRetryAfter, "30")
		return problemDetailCode(c, fiber.StatusBadGateway, "Bad Gateway",
			"Google Wallet did not answer; try again.", "skypass_google_wallet_unavailable")
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, skypass.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, skypass.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, skypass.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, skypass.ErrExpired):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, skypass.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

// JWKS is SkyPass's key (sp-e1) and the Ticket QR's (tk-e1, and the
// previous one during a rotation's window): a scanner picks the key by the
// token's kid (docs/ticket-qr.md). Also served as /v1/door/jwks.
func (h *SkyPassHandler) JWKS(c fiber.Ctx) error {
	set := h.svc.JWKS()
	if h.ticketKeys != nil {
		for _, k := range h.ticketKeys.JWKs() {
			set.Keys = append(set.Keys, skypass.JWK{Kty: k.Kty, Kid: k.Kid, Alg: k.Alg, Use: k.Use, Crv: k.Crv, X: k.X, Y: k.Y})
		}
	}
	return c.JSON(set)
}

func (h *SkyPassHandler) BindCard(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	var body cardBindBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.BindCard(c.Context(), p, body.UID, body.UserID)
	if err != nil {
		return skypassError(c, err)
	}
	return c.JSON(got)
}

// UnbindCard removes the caller's own bound card (docs/skypass-card.md).
func (h *SkyPassHandler) UnbindCard(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	if err := h.svc.UnbindCard(c.Context(), p); err != nil {
		return skypassError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *SkyPassHandler) LookupCard(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	got, err := h.svc.Lookup(c.Context(), p, c.Query("uid"))
	if err != nil {
		return skypassError(c, err)
	}
	return c.JSON(got)
}

func (h *SkyPassHandler) Mint(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	tok, err := h.svc.Mint(c.Context(), p)
	if err != nil {
		return skypassError(c, err)
	}
	return c.JSON(tok)
}

func (h *SkyPassHandler) Verify(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	var body verifyBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.Verify(c.Context(), p, body.Token)
	if err != nil {
		return skypassError(c, err)
	}
	return c.JSON(got)
}

func (h *SkyPassHandler) CheckInSession(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body settleBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if h.tickets == nil {
		return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
	}
	ctx, ok, answered := scanContext(c, body)
	if !ok {
		return answered
	}
	// The door first: reading a Google Wallet code spends it, and the
	// answer to someone who may not scan here must not depend on whose
	// credential it is (no Ticket → 404 would tell).
	if err := h.tickets.AuthorizeSessionDoor(ctx, p, sessionID); err != nil {
		return ticketError(c, err)
	}
	// A Ticket QR (kid tk-e1) goes to its Ticket; anything else is SkyPass's
	// as before: its QR (sp-e1), a Google Wallet code or a card UID.
	if body.Token != "" && ticketqr.IsTicketQR(body.Token) {
		ci, err := h.tickets.CheckInTicketQR(ctx, p, sessionID, body.Token)
		if handled, answer := offlineDuplicate(c, err); handled {
			return answer
		}
		if err != nil {
			return ticketQRError(c, err)
		}
		return c.Status(fiber.StatusCreated).JSON(ci)
	}
	got, err := h.svc.HolderFrom(ctx, p, body.Token, body.UID)
	if err != nil {
		return skypassError(c, err)
	}
	ci, err := h.tickets.CheckInUser(ctx, p, sessionID, got.ID)
	if handled, answer := offlineDuplicate(c, err); handled {
		return answer
	}
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

// EnterEvent is POST /v1/events/:eventId/entries/scan: today's Venue entry
// of whatever the door scanned, no Session picked (docs/venue-entry.md).
// As CheckInSession: the door first, then the code.
func (h *SkyPassHandler) EnterEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body settleBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if h.tickets == nil {
		return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
	}
	ctx, ok, answered := scanContext(c, body)
	if !ok {
		return answered
	}
	if err := h.tickets.AuthorizeEventDoor(ctx, p, eventID); err != nil {
		return ticketError(c, err)
	}
	if body.Token != "" && ticketqr.IsTicketQR(body.Token) {
		entry, err := h.tickets.EnterTicketQR(ctx, p, eventID, body.Token)
		if handled, answer := offlineDuplicate(c, err); handled {
			return answer
		}
		if err != nil {
			return ticketQRError(c, err)
		}
		return c.Status(fiber.StatusCreated).JSON(entry)
	}
	got, err := h.svc.HolderFrom(ctx, p, body.Token, body.UID)
	if err != nil {
		return skypassError(c, err)
	}
	entry, err := h.tickets.EnterUser(ctx, p, eventID, got.ID)
	if handled, answer := offlineDuplicate(c, err); handled {
		return answer
	}
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(entry)
}

// ticketQRError answers a Ticket QR scan's refusal (docs/ticket-qr.md).
func ticketQRError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, ticket.ErrTicketQRInvalid):
		return problemDetailCode(c, fiber.StatusUnauthorized, "Unauthorized", "Bilet kodu geçersiz.", "ticket_qr_invalid")
	case errors.Is(err, ticket.ErrTicketQRExpired):
		return problemDetailCode(c, fiber.StatusUnauthorized, "Unauthorized", "Bilet kodunun süresi dolmuş.", "ticket_qr_expired")
	case errors.Is(err, ticket.ErrTicketQRSuperseded):
		return problemDetailCode(c, fiber.StatusGone, "Gone",
			"Bu bilet yenilenmiş; kişiden son postadaki bileti açmasını iste.", "ticket_qr_superseded")
	default:
		return ticketError(c, err)
	}
}

// WalletStatus tells the caller's app whether to offer "Add to Google
// Wallet" (docs/skypass-google-wallet.md).
func (h *SkyPassHandler) WalletStatus(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	got, err := h.svc.WalletStatus(c.Context(), p)
	if err != nil {
		return skypassError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(got)
}

// GoogleWalletLink answers the caller's "Add to Google Wallet" link.
func (h *SkyPassHandler) GoogleWalletLink(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	got, err := h.svc.GoogleWalletLink(c.Context(), p)
	if err != nil {
		return skypassError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(got)
}

// RevokeGoogleWallet ends the caller's Wallet pass.
func (h *SkyPassHandler) RevokeGoogleWallet(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	if err := h.svc.RevokeGoogleWallet(c.Context(), p); err != nil {
		return skypassError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// SkyPassWalletLinksPerMinute is how many save links (and revocations) one
// person may ask for per minute: each one writes to Google.
const SkyPassWalletLinksPerMinute = 10

// SkyPassWalletLimit budgets the Wallet writes by the caller's token
// subject. Requests without an identity pass through to the handler, which
// answers 401.
func SkyPassWalletLimit() fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        SkyPassWalletLinksPerMinute,
		Expiration: time.Minute,
		Next: func(c fiber.Ctx) bool {
			_, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
			return !ok
		},
		KeyGenerator: func(c fiber.Ctx) string {
			ident, _ := c.Locals(authn.LocalsIdentity).(authn.Identity)
			return ident.ID.String()
		},
		LimitReached: func(c fiber.Ctx) error {
			seconds, _ := strconv.Atoi(string(c.Response().Header.Peek(fiber.HeaderRetryAfter)))
			return problemWithFields(c, fiber.StatusTooManyRequests, "Too Many Requests",
				"Too many requests; retry after the given seconds.", "skypass_wallet_link_rate_limited", fiber.Map{"retryAfterSeconds": seconds})
		},
	})
}
