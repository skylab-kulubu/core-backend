package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

type AccountDeletionService interface {
	Begin(context.Context, uuid.UUID, string) (account.SelfDeletionView, error)
	Status(context.Context, string) (account.SelfDeletionView, error)
	Retry(context.Context, string) (account.SelfDeletionView, error)
}

type AccountDeletionHandler struct {
	service    AccountDeletionService
	parseToken func(string, string) (authn.Identity, error)
}

func NewAccountDeletionHandler(service AccountDeletionService, parseToken func(string, string) (authn.Identity, error)) *AccountDeletionHandler {
	return &AccountDeletionHandler{service: service, parseToken: parseToken}
}

type accountDeletionResponse struct {
	Receipt          string                     `json:"receipt,omitempty"`
	Status           account.SelfDeletionStatus `json:"status"`
	Partial          bool                       `json:"partial"`
	PlatformBlocked  bool                       `json:"platformBlocked"`
	RequestedAt      time.Time                  `json:"requestedAt"`
	UpdatedAt        time.Time                  `json:"updatedAt"`
	CompletedAt      *time.Time                 `json:"completedAt"`
	ReceiptExpiresAt time.Time                  `json:"receiptExpiresAt"`
}

func deletionResponse(view account.SelfDeletionView, includeReceipt bool) accountDeletionResponse {
	receipt := ""
	if includeReceipt {
		receipt = view.Receipt
	}
	return accountDeletionResponse{
		Receipt: receipt, Status: view.Status, Partial: view.Partial,
		PlatformBlocked: view.PlatformBlocked, RequestedAt: view.RequestedAt,
		UpdatedAt: view.UpdatedAt, CompletedAt: view.CompletedAt,
		ReceiptExpiresAt: view.ReceiptExpiresAt,
	}
}

func (h *AccountDeletionHandler) Begin(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	identity, err := h.endUserIdentity(c)
	if err != nil {
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
		return problemCode(c, fiber.StatusUnauthorized, "Unauthorized", "invalid_end_user_token")
	}
	if len(c.Body()) != 0 {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "invalid_request")
	}
	view, err := h.service.Begin(c.Context(), identity.ID, c.Get("Idempotency-Key"))
	if err != nil {
		return accountDeletionError(c, err)
	}
	return c.Status(accountDeletionHTTPStatus(view)).JSON(deletionResponse(view, true))
}

func (h *AccountDeletionHandler) Status(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	receipt, ok := deletionReceipt(c.Get(fiber.HeaderAuthorization))
	if !ok {
		return problemCode(c, fiber.StatusNotFound, "Not Found", "account_deletion_receipt_not_found")
	}
	view, err := h.service.Status(c.Context(), receipt)
	if err != nil {
		return accountDeletionError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(deletionResponse(view, false))
}

func (h *AccountDeletionHandler) Retry(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	if len(c.Body()) != 0 {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "invalid_request")
	}
	receipt, ok := deletionReceipt(c.Get(fiber.HeaderAuthorization))
	if !ok {
		return problemCode(c, fiber.StatusNotFound, "Not Found", "account_deletion_receipt_not_found")
	}
	view, err := h.service.Retry(c.Context(), receipt)
	if err != nil {
		return accountDeletionError(c, err)
	}
	return c.Status(accountDeletionHTTPStatus(view)).JSON(deletionResponse(view, false))
}

func (h *AccountDeletionHandler) endUserIdentity(c fiber.Ctx) (authn.Identity, error) {
	if h == nil || h.service == nil || h.parseToken == nil {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	token, ok := authorizationCredential(c.Get(fiber.HeaderAuthorization), "Bearer")
	if !ok {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	reauthenticationToken := c.Get("X-Account-Reauth-Token")
	if reauthenticationToken == "" || strings.TrimSpace(reauthenticationToken) != reauthenticationToken || strings.ContainsAny(reauthenticationToken, " \t\r\n,") {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	return h.parseToken(token, reauthenticationToken)
}

func deletionReceipt(header string) (string, bool) {
	return authorizationCredential(header, "DeletionReceipt")
}

func authorizationCredential(header, scheme string) (string, bool) {
	prefix := scheme + " "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	credential := strings.TrimPrefix(header, prefix)
	if credential == "" || strings.TrimSpace(credential) != credential || strings.ContainsAny(credential, " \t\r\n,") {
		return "", false
	}
	return credential, true
}

func accountDeletionHTTPStatus(view account.SelfDeletionView) int {
	if view.Status == account.SelfDeletionCompleted || view.Status == account.SelfDeletionManualIntervention {
		return fiber.StatusOK
	}
	return fiber.StatusAccepted
}

func accountDeletionError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, account.ErrSelfDeletionInvalidIdempotencyKey):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "invalid_idempotency_key")
	case errors.Is(err, account.ErrSelfDeletionIdempotencyConflict):
		return problemCode(c, fiber.StatusConflict, "Conflict", "idempotency_conflict")
	case errors.Is(err, account.ErrSelfDeletionReceiptNotFound):
		return problemCode(c, fiber.StatusNotFound, "Not Found", "account_deletion_receipt_not_found")
	case errors.Is(err, account.ErrSelfDeletionDisabled), errors.Is(err, account.ErrSelfDeletionUnavailable):
		c.Set(fiber.HeaderRetryAfter, "1")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "account_deletion_unavailable")
	default:
		return err
	}
}
