package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// UserAccountHandler answers the person card's account routes
// (docs/user-accounts.md): GET /v1/users/{id}/account and the writes under
// it. Every write's audit row carries the caller (auditContext).
type UserAccountHandler struct {
	svc *identity.UserAccounts
}

func NewUserAccountHandler(svc *identity.UserAccounts) *UserAccountHandler {
	return &UserAccountHandler{svc: svc}
}

var accountTitles = map[int]string{
	fiber.StatusForbidden:           "Forbidden",
	fiber.StatusNotFound:            "Not Found",
	fiber.StatusConflict:            "Conflict",
	fiber.StatusUnprocessableEntity: "Unprocessable Content",
	fiber.StatusBadGateway:          "Bad Gateway",
}

// accountProblem answers a refused account request with its status and
// code; anything else as the identity routes do.
func accountProblem(c fiber.Ctx, err error) error {
	var refused *identity.AccountError
	if errors.As(err, &refused) {
		title, ok := accountTitles[refused.Status]
		if !ok {
			title = "Conflict"
		}
		var fields fiber.Map
		if refused.Policy != "" {
			fields = fiber.Map{"policy": refused.Policy}
		}
		return problemWithFields(c, refused.Status, title, title, refused.Code, fields)
	}
	return identityError(c, err)
}

// target reads the person's id; a bad one is answered 400.
func (h *UserAccountHandler) target(c fiber.Ctx) (uuid.UUID, bool, error) {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return uuid.Nil, false, problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return id, true, nil
}

// decode reads a JSON body strictly: unknown fields and trailing data are
// refused.
func decode(c fiber.Ctx, out any) bool {
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out) == nil && !decoder.More()
}

// Account answers GET /v1/users/{id}/account.
func (h *UserAccountHandler) Account(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	id, ok, err := h.target(c)
	if !ok {
		return err
	}
	account, err := h.svc.Account(c.Context(), p, id)
	if err != nil {
		return accountProblem(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(account)
}

// run reads the caller and the person, runs do with the caller in the
// audit context, and answers 204 for its success.
func (h *UserAccountHandler) run(c fiber.Ctx, do func(context.Context, authz.Principal, uuid.UUID) error) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	id, ok, err := h.target(c)
	if !ok {
		return err
	}
	if err := do(auditContext(c, p), p, id); err != nil {
		return accountProblem(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// SetEnabled answers PUT /v1/users/{id}/enabled {enabled}.
func (h *UserAccountHandler) SetEnabled(c fiber.Ctx) error {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(c, &body) || body.Enabled == nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.SetEnabled(ctx, p, id, *body.Enabled)
	})
}

// SetPassword answers POST /v1/users/{id}/password {password?}: a temporary
// password, made up by core when none is given and then answered once.
// The answer is never stored by a cache.
func (h *UserAccountHandler) SetPassword(c fiber.Ctx) error {
	var body struct {
		Password string `json:"password"`
	}
	if len(c.Body()) > 0 && !decode(c, &body) {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	id, ok, err := h.target(c)
	if !ok {
		return err
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	result, err := h.svc.SetTemporaryPassword(auditContext(c, p), p, id, body.Password)
	if err != nil {
		return accountProblem(c, err)
	}
	return c.JSON(result)
}

// SendPasswordMail answers POST /v1/users/{id}/password-reset-mail.
func (h *UserAccountHandler) SendPasswordMail(c fiber.Ctx) error {
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.SendPasswordMail(ctx, p, id)
	})
}

// SendVerificationMail answers POST /v1/users/{id}/email/verification-mail.
func (h *UserAccountHandler) SendVerificationMail(c fiber.Ctx) error {
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.SendVerificationMail(ctx, p, id)
	})
}

// ChangeEmail answers PUT /v1/users/{id}/email {address, verified, primary}.
func (h *UserAccountHandler) ChangeEmail(c fiber.Ctx) error {
	var body struct {
		Address  string `json:"address"`
		Verified bool   `json:"verified"`
		Primary  bool   `json:"primary"`
	}
	if !decode(c, &body) {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.ChangeEmail(ctx, p, id, identity.EmailChange{
			Address: body.Address, Verified: body.Verified, Primary: body.Primary,
		})
	})
}

// VerifyEmail answers POST /v1/users/{id}/email/verify {kind?}: the
// Primary e-mail without a body, the Personal e-mail with kind "personal".
func (h *UserAccountHandler) VerifyEmail(c fiber.Ctx) error {
	var body struct {
		Kind string `json:"kind"`
	}
	if len(c.Body()) > 0 && !decode(c, &body) {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.VerifyEmail(ctx, p, id, body.Kind)
	})
}

// SetPrimary answers PUT /v1/users/{id}/email/primary {kind}.
func (h *UserAccountHandler) SetPrimary(c fiber.Ctx) error {
	var body struct {
		Kind string `json:"kind"`
	}
	if !decode(c, &body) {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.SetPrimary(ctx, p, id, body.Kind)
	})
}

// SetRequiredActions answers PUT /v1/users/{id}/required-actions {actions}.
func (h *UserAccountHandler) SetRequiredActions(c fiber.Ctx) error {
	var body struct {
		Actions []string `json:"actions"`
	}
	if !decode(c, &body) || body.Actions == nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.SetRequiredActions(ctx, p, id, body.Actions)
	})
}

// DeleteCredential answers DELETE /v1/users/{id}/credentials/{credentialId}.
func (h *UserAccountHandler) DeleteCredential(c fiber.Ctx) error {
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.DeleteCredential(ctx, p, id, c.Params("credentialId"))
	})
}

// UnlinkIdentity answers DELETE /v1/users/{id}/identity-providers/{provider}.
func (h *UserAccountHandler) UnlinkIdentity(c fiber.Ctx) error {
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.UnlinkIdentity(ctx, p, id, c.Params("provider"))
	})
}

// ClearLockout answers DELETE /v1/users/{id}/lockout.
func (h *UserAccountHandler) ClearLockout(c fiber.Ctx) error {
	return h.run(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) error {
		return h.svc.ClearLockout(ctx, p, id)
	})
}
