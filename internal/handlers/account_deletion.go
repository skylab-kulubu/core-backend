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
	// Replay returns the outcome of a key already accepted for the subject,
	// or account.ErrSelfDeletionNotAccepted.
	Replay(context.Context, uuid.UUID, string) (account.SelfDeletionView, error)
	Status(context.Context, string) (account.SelfDeletionView, error)
	Retry(context.Context, string) (account.SelfDeletionView, error)
}

type AccountDeletionHandler struct {
	service     AccountDeletionService
	parseToken  func(string, string) (authn.Identity, error)
	parseSudo   func(context.Context, string, string) (authn.Identity, error)
	parseBearer func(string) (authn.Identity, error)
}

// NewAccountDeletionHandler takes one verifier per re-authentication proof:
// parseToken checks the bearer with a fresh ID token (`X-Account-Reauth-Token`)
// and parseSudo checks it with a sky-account Sudo mode token (`X-Sky-Sudo`).
// parseBearer checks the bearer alone, with the sudo path's local rules, so
// that a replay of an already accepted key can be answered without asking
// the realm about a proof whose session the deletion has closed. A nil
// parseSudo refuses every sudo proof; a nil parseSudo or parseBearer turns
// the replay off.
func NewAccountDeletionHandler(service AccountDeletionService, parseToken func(string, string) (authn.Identity, error), parseSudo func(context.Context, string, string) (authn.Identity, error), parseBearer func(string) (authn.Identity, error)) *AccountDeletionHandler {
	return &AccountDeletionHandler{service: service, parseToken: parseToken, parseSudo: parseSudo, parseBearer: parseBearer}
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
	if view, replayed, err := h.replayAccepted(c); err != nil {
		return accountDeletionError(c, err)
	} else if replayed {
		return c.Status(accountDeletionHTTPStatus(view)).JSON(deletionResponse(view, true))
	}
	identity, err := h.endUserIdentity(c)
	if errors.Is(err, authn.ErrIntrospectionUnavailable) {
		// The realm could not say whether the sudo proof is good. That is not
		// a refusal: the person's proof may be fine, so they try again.
		c.Set(fiber.HeaderRetryAfter, "1")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "account_deletion_unavailable")
	}
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

// replayAccepted answers a sudo-path retry of an idempotency key Core already
// accepted for the bearer's subject, before the sudo proof is checked. The
// deletion saga closes the Keycloak session that proof is bound to, so the
// realm calls it inactive while Account Center may still be retrying a lost
// answer. Only a request shaped exactly like the sudo intake qualifies - a
// verified Account Center bearer, one well-formed `X-Sky-Sudo`, an empty
// body - and it can only read the stored outcome: a key Core never accepted
// for this subject falls through to the proof, and the ID-token path never
// replays. replayed=false with a nil error means "not a replay".
func (h *AccountDeletionHandler) replayAccepted(c fiber.Ctx) (account.SelfDeletionView, bool, error) {
	if h == nil || h.service == nil || h.parseSudo == nil || h.parseBearer == nil {
		return account.SelfDeletionView{}, false, nil
	}
	if !singleCredential(c.Get("X-Sky-Sudo")) || len(c.Body()) != 0 {
		return account.SelfDeletionView{}, false, nil
	}
	token, ok := authorizationCredential(c.Get(fiber.HeaderAuthorization), "Bearer")
	if !ok {
		return account.SelfDeletionView{}, false, nil
	}
	identity, err := h.parseBearer(token)
	if err != nil {
		return account.SelfDeletionView{}, false, nil
	}
	view, err := h.service.Replay(c.Context(), identity.ID, c.Get("Idempotency-Key"))
	if errors.Is(err, account.ErrSelfDeletionNotAccepted) {
		return account.SelfDeletionView{}, false, nil
	}
	if err != nil {
		return account.SelfDeletionView{}, false, err
	}
	return view, true, nil
}

func (h *AccountDeletionHandler) endUserIdentity(c fiber.Ctx) (authn.Identity, error) {
	if h == nil || h.service == nil || h.parseToken == nil {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	token, ok := authorizationCredential(c.Get(fiber.HeaderAuthorization), "Bearer")
	if !ok {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	// Account Center may send both proofs while it moves from the ID token to
	// Sudo mode. The sudo token then decides alone: it is the proof the person
	// just made, and a refused sudo token is not rescued by the other header.
	if sudoToken := c.Get("X-Sky-Sudo"); sudoToken != "" {
		if !singleCredential(sudoToken) || h.parseSudo == nil {
			return authn.Identity{}, authn.ErrInvalidToken
		}
		return h.parseSudo(c.Context(), token, sudoToken)
	}
	reauthenticationToken := c.Get("X-Account-Reauth-Token")
	if !singleCredential(reauthenticationToken) {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	return h.parseToken(token, reauthenticationToken)
}

// singleCredential reports whether a header carries exactly one non-empty
// token with nothing around it.
func singleCredential(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, " \t\r\n,")
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
	if !singleCredential(credential) {
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
