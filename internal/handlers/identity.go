package handlers

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
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
	// UnverifiedAck: the caller confirmed the warning that the person is not
	// YTÜ-linked (grup-yonetimi 07). Optional; it never decides the add.
	UnverifiedAck bool `json:"unverifiedAck"`
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
		Product: ident.Product, Client: ident.Client, ServiceAccount: ident.ServiceAccount,
	}, nil
}

// auditUnavailableCode answers a Group write whose audit row could not be
// opened: the change was not made (docs/team-audit.md).
const auditUnavailableCode = "audit_unavailable"

// auditContext carries the caller of a Group write to the Team audit record
// (docs/team-audit.md): their subject, the client their token was issued to
// and the request's id, as Privileged; a delegated write names its Leader
// basis itself (docs/team-delegation.md). A caller whose subject is not an
// id carries no actor, and the write is refused (503).
func auditContext(c fiber.Ctx, p authz.Principal) context.Context {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return c.Context()
	}
	return teamaudit.WithActor(c.Context(), teamaudit.Actor{
		ID: id, Basis: teamaudit.BasisPrivileged, Client: p.Client, CorrelationID: requestid.FromContext(c),
	})
}

// callerOrNil is the signed-in caller, nil when signed out: who a read that
// hides drafts answers.
func callerOrNil(c fiber.Ctx) *authz.Principal {
	p, err := caller(c)
	if err != nil {
		return nil
	}
	return &p
}

// optionalCaller is the signed-in caller, or the zero Principal when the
// request carries no identity.
func optionalCaller(c fiber.Ctx) authz.Principal {
	p, err := caller(c)
	if err != nil {
		return authz.Principal{}
	}
	return p
}

func identityError(c fiber.Ctx, err error) error {
	var deletion *identity.GroupDeleteError
	var name *teamcode.Error
	var refused *identity.DelegationError
	var rule *identity.RoleRuleError
	switch {
	case errors.As(err, &deletion):
		if deletion.Status == fiber.StatusServiceUnavailable {
			c.Set(fiber.HeaderCacheControl, "no-store")
			c.Set(fiber.HeaderRetryAfter, "1")
		}
		return problemWithFields(c, deletion.Status, delegationTitles[deletion.Status], deletion.Detail, deletion.Code,
			fiber.Map{"members": deletion.Members, "children": deletion.Children})
	case errors.As(err, &rule):
		return roleRuleError(c, rule)
	case errors.As(err, &name):
		return groupNameError(c, name)
	case errors.As(err, &refused):
		return delegationError(c, refused)
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
	case errors.Is(err, teamaudit.ErrUnavailable), errors.Is(err, teamcode.ErrUnavailable):
		// The change was not made: without its audit row there is none.
		c.Set(fiber.HeaderCacheControl, "no-store")
		c.Set(fiber.HeaderRetryAfter, "1")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", auditUnavailableCode)
	default:
		return err
	}
}

// delegationTitles are the titles of a delegated refusal's statuses.
var delegationTitles = map[int]string{
	fiber.StatusForbidden:          "Forbidden",
	fiber.StatusConflict:           "Conflict",
	fiber.StatusTooManyRequests:    "Too Many Requests",
	fiber.StatusServiceUnavailable: "Service Unavailable",
}

// delegationError answers a delegated request core refused
// (docs/team-delegation.md) with its status and problem code.
func delegationError(c fiber.Ctx, err *identity.DelegationError) error {
	title, ok := delegationTitles[err.Status]
	if !ok {
		return err
	}
	if err.RetryAfter > 0 {
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(err.RetryAfter))
	}
	if err.Status == fiber.StatusServiceUnavailable {
		c.Set(fiber.HeaderCacheControl, "no-store")
	}
	return problemCode(c, err.Status, title, err.Code)
}

// roleRuleError answers a request a role rule refused (docs/team-delegation.md,
// "Role rules"): 403 direct_role_not_allowed or keycloak_admin_group, 409
// role_placement with the placement's reason.
func roleRuleError(c fiber.Ctx, err *identity.RoleRuleError) error {
	title, ok := delegationTitles[err.Status]
	if !ok {
		return err
	}
	var fields fiber.Map
	if err.Reason != "" {
		fields = fiber.Map{"reason": err.Reason}
	}
	return problemWithFields(c, err.Status, title, err.Detail, err.Code, fields)
}

// groupNameError answers a Group name the Team code rule refused
// (docs/team-codes.md): a name that is not a code or is reserved cannot be
// processed (422), one that is taken or would move a Privileged root
// conflicts with what exists (409).
func groupNameError(c fiber.Ctx, err *teamcode.Error) error {
	switch err.Code {
	case teamcode.NameTaken, teamcode.PrivilegedRoot:
		return problemDetailCode(c, fiber.StatusConflict, "Conflict", err.Detail, err.Code)
	default:
		return problemDetailCode(c, fiber.StatusUnprocessableEntity, "Unprocessable Content", err.Detail, err.Code)
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
	created, err := h.svc.CreateGroup(auditContext(c, p), p, body.ParentID, body.Name)
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
	updated, err := h.svc.UpdateGroup(auditContext(c, p), p, c.Params("groupId"), body.Name, body.Attributes)
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(updated)
}

func (h *IdentityHandler) DeleteGroup(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	if err := h.svc.DeleteGroup(auditContext(c, p), p, c.Params("groupId")); err != nil {
		return identityError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
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

// ManagedGroups answers GET /v1/users/me/managed-groups: the Teams the
// caller manages, as a tree (docs/team-delegation.md).
func (h *IdentityHandler) ManagedGroups(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	nodes, err := h.svc.ManagedGroups(c.Context(), p)
	if err != nil {
		return identityError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(nodes)
}

// SetTeamSettings answers PUT /v1/groups/{id}/settings: a Team's own
// settings, written by its manager or Privileged (docs/team-delegation.md
// "Team settings"). Every field is required.
func (h *IdentityHandler) SetTeamSettings(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var body struct {
		MembersManageCompetitors *bool `json:"membersManageCompetitors"`
	}
	if err := c.Bind().Body(&body); err != nil || body.MembersManageCompetitors == nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	settings, err := h.svc.SetTeamSettings(auditContext(c, p), p, c.Params("groupId"), identity.TeamSettings{
		MembersManageCompetitors: *body.MembersManageCompetitors,
	})
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(settings)
}

// MemberCandidates answers GET /v1/groups/{id}/member-candidates?q=.
func (h *IdentityHandler) MemberCandidates(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	found, err := h.svc.MemberCandidates(c.Context(), p, c.Params("groupId"), c.Query("q"))
	if err != nil {
		return identityError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(found)
}

// MemberCheck answers GET /v1/groups/{id}/member-check/{userId}: what adding
// the person to the Group would warn of (grup-yonetimi 07).
func (h *IdentityHandler) MemberCheck(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	userID, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	check, err := h.svc.MemberCheck(c.Context(), p, c.Params("groupId"), userID)
	if err != nil {
		return identityError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(check)
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
	ctx := identity.WithUnverifiedAck(auditContext(c, p), body.UnverifiedAck)
	if err := h.svc.AddMember(ctx, p, c.Params("groupId"), body.UserID); err != nil {
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
	if err := h.svc.RemoveMember(auditContext(c, p), p, c.Params("groupId"), userID); err != nil {
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
	if err := h.svc.SetGroupClientRoles(auditContext(c, p), p, c.Params("groupId"), roles); err != nil {
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
