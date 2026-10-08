package handlers

import (
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type SkyPassHandler struct {
	svc     skypass.Service
	tickets ticket.Service
}

func NewSkyPassHandler(svc skypass.Service, tickets ticket.Service) *SkyPassHandler {
	return &SkyPassHandler{svc: svc, tickets: tickets}
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

func (h *SkyPassHandler) JWKS(c fiber.Ctx) error {
	return c.JSON(h.svc.JWKS())
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
	got, err := h.svc.HolderFrom(c.Context(), p, body.Token, body.UID)
	if err != nil {
		return skypassError(c, err)
	}
	if h.tickets == nil {
		return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
	}
	ci, err := h.tickets.CheckInUser(c.Context(), p, sessionID, got.ID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
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
