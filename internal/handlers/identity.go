package handlers

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
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
	return authz.Principal{
		ID: ident.ID.String(), Groups: ident.Groups, Roles: ident.Roles,
		Product: ident.Product,
	}, nil
}

func identityError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, identity.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, identity.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, identity.ErrAccountErasureDisabled), errors.Is(err, identity.ErrAccountAccessUnavailable):
		c.Set(fiber.HeaderCacheControl, "no-store")
		c.Set(fiber.HeaderRetryAfter, "1")
		return problem(c, fiber.StatusServiceUnavailable, "Service Unavailable")
	case errors.Is(err, identity.ErrInvalid), errors.Is(err, user.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	case errors.Is(err, user.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, user.ErrAccountBlocked):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, user.ErrYTUManaged):
		// Admins do not override the YTÜ values either: the person's next
		// YTÜ login would silently put them back.
		return problemCode(c, fiber.StatusConflict, "Conflict", ytuManagedCode)
	case errors.Is(err, user.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
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

func (h *IdentityHandler) GetGroup(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	g, err := h.svc.GetGroup(c.Context(), p, c.Params("groupId"))
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(g)
}

func (h *IdentityHandler) CreateGroup(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var body struct {
		Name     string `json:"name"`
		ParentID string `json:"parentId"`
	}
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.CreateGroup(c.Context(), p, body.ParentID, body.Name)
	if err != nil {
		return identityError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *IdentityHandler) UpdateGroup(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var body struct {
		Name       string            `json:"name"`
		Attributes map[string]string `json:"attributes"`
	}
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.svc.UpdateGroup(c.Context(), p, c.Params("groupId"), body.Name, body.Attributes)
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(updated)
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
	if members == nil {
		members = []identity.GroupMember{}
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

func (h *IdentityHandler) ListClientRoles(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	roles, err := h.svc.ListClientRoles(c.Context(), p)
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(roles)
}

func (h *IdentityHandler) ListUsers(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var seat []identity.ClientRole
	if role := strings.TrimSpace(c.Query("role")); role != "" {
		if !strings.HasPrefix(role, "skyforms:") {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		seat = []identity.ClientRole{{ClientID: "forms", Role: role}}
	}
	users, err := h.svc.ListUsers(c.Context(), p, c.Query("q"), seat...)
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(users)
}

func (h *IdentityHandler) GetUser(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	card, err := h.svc.GetUser(c.Context(), p, id)
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(card)
}

func (h *IdentityHandler) PatchUser(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body struct {
		FirstName  *string `json:"firstName"`
		LastName   *string `json:"lastName"`
		Linkedin   *string `json:"linkedin"`
		University *string `json:"university"`
		Faculty    *string `json:"faculty"`
		Department *string `json:"department"`
		Phone      *string `json:"phone"`
	}
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	card, err := h.svc.PatchUser(c.Context(), p, id, user.ProfilePatch{
		FirstName:  body.FirstName,
		LastName:   body.LastName,
		Linkedin:   body.Linkedin,
		University: body.University,
		Faculty:    body.Faculty,
		Department: body.Department,
		Phone:      body.Phone,
	})
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(card)
}

func (h *IdentityHandler) GroupClientRoles(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	roles, err := h.svc.GroupClientRoles(c.Context(), p, c.Params("groupId"))
	if err != nil {
		return identityError(c, err)
	}
	if roles == nil {
		roles = []identity.ClientRole{}
	}
	return c.JSON(roles)
}

func (h *IdentityHandler) SetGroupClientRoles(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var roles []identity.ClientRole
	if err := c.Bind().Body(&roles); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.SetGroupClientRoles(c.Context(), p, c.Params("groupId"), roles); err != nil {
		return identityError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *IdentityHandler) AddUserExtraRole(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var role identity.ClientRole
	if err := c.Bind().Body(&role); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.AddUserExtraRole(c.Context(), p, id, role); err != nil {
		return identityError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *IdentityHandler) RemoveUserExtraRole(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	role := identity.ClientRole{ClientID: c.Query("clientId"), Role: c.Query("role")}
	if err := h.svc.RemoveUserExtraRole(c.Context(), p, id, role); err != nil {
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

func (h *IdentityHandler) LogoutAllSessions(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.LogoutAllSessions(c.Context(), p, id); err != nil {
		return identityError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
