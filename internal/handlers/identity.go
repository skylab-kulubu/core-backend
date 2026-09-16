package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

type IdentityHandler struct {
	svc identity.Service
}

func NewIdentityHandler(svc identity.Service) *IdentityHandler {
	return &IdentityHandler{svc: svc}
}

type addMemberBody struct {
	UserID uuid.UUID `json:"userId"`
}

type createUserBody struct {
	Email     string `json:"email"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
}

func caller(c fiber.Ctx) (authz.Principal, error) {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return authz.Principal{}, fiber.ErrUnauthorized
	}
	return authz.Principal{ID: ident.ID.String(), Groups: ident.Groups}, nil
}

func identityError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, identity.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, identity.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, identity.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (h *IdentityHandler) ListGroups(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	groups, err := h.svc.ListGroups(c.Context(), p)
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(groups)
}

func (h *IdentityHandler) Members(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	members, err := h.svc.Members(c.Context(), p, c.Params("groupId"))
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(members)
}

func (h *IdentityHandler) AddMember(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var body addMemberBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if body.UserID == uuid.Nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.AddMember(c.Context(), p, c.Params("groupId"), body.UserID); err != nil {
		return identityError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *IdentityHandler) RemoveMember(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	userID, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.RemoveMember(c.Context(), p, c.Params("groupId"), userID); err != nil {
		return identityError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *IdentityHandler) CreateUser(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var body createUserBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.CreateUser(c.Context(), p, identity.Person{
		Email:     body.Email,
		FirstName: body.FirstName,
		LastName:  body.LastName,
	})
	if err != nil {
		return identityError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *IdentityHandler) DeleteUser(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.DeleteUser(c.Context(), p, id); err != nil {
		return identityError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
