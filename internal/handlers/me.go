package handlers

import (
	"errors"
	"io"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type MeHandler struct {
	users user.Service
	media media.Service
}

func NewMeHandler(users user.Service, media media.Service) *MeHandler {
	return &MeHandler{users: users, media: media}
}

type meBody struct {
	FirstName  string `json:"firstName"`
	LastName   string `json:"lastName"`
	Linkedin   string `json:"linkedin"`
	University string `json:"university"`
	Faculty    string `json:"faculty"`
	Department string `json:"department"`
}

type mePatchBody struct {
	FirstName  *string `json:"firstName"`
	LastName   *string `json:"lastName"`
	Linkedin   *string `json:"linkedin"`
	University *string `json:"university"`
	Faculty    *string `json:"faculty"`
	Department *string `json:"department"`
}

func meError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, user.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, user.ErrInvalid), errors.Is(err, media.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	case errors.Is(err, media.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	default:
		return err
	}
}

func (h *MeHandler) GetMe(c fiber.Ctx) error {
	u, ok := c.Locals(authn.LocalsUser).(user.User)
	if !ok {
		return fiber.ErrUnauthorized
	}
	return c.JSON(publicUser(u))
}

func (h *MeHandler) identityID(c fiber.Ctx) (user.User, error) {
	u, ok := c.Locals(authn.LocalsUser).(user.User)
	if !ok {
		return user.User{}, fiber.ErrUnauthorized
	}
	return u, nil
}

func (h *MeHandler) PutMe(c fiber.Ctx) error {
	u, err := h.identityID(c)
	if err != nil {
		return meError(c, err)
	}
	var body meBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.users.Replace(c.Context(), u.ID, user.ProfileUpdate{
		FirstName:  body.FirstName,
		LastName:   body.LastName,
		Linkedin:   body.Linkedin,
		University: body.University,
		Faculty:    body.Faculty,
		Department: body.Department,
	})
	if err != nil {
		return meError(c, err)
	}
	return c.JSON(publicUser(updated))
}

func (h *MeHandler) PatchMe(c fiber.Ctx) error {
	u, err := h.identityID(c)
	if err != nil {
		return meError(c, err)
	}
	var body mePatchBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.users.Patch(c.Context(), u.ID, user.ProfilePatch{
		FirstName:  body.FirstName,
		LastName:   body.LastName,
		Linkedin:   body.Linkedin,
		University: body.University,
		Faculty:    body.Faculty,
		Department: body.Department,
	})
	if err != nil {
		return meError(c, err)
	}
	return c.JSON(publicUser(updated))
}

func (h *MeHandler) ProfilePicture(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return meError(c, err)
	}
	u, err := h.identityID(c)
	if err != nil {
		return meError(c, err)
	}
	header, err := c.FormFile("image")
	if err != nil || header == nil {
		header, err = c.FormFile("file")
	}
	if err != nil || header == nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	f, err := header.Open()
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	uploaded, err := h.media.Upload(c.Context(), p, header.Filename, header.Header.Get("Content-Type"), data)
	if err != nil {
		return meError(c, err)
	}
	updated, err := h.users.SetProfilePicture(c.Context(), u.ID, uploaded.ID, uploaded.URL)
	if err != nil {
		return meError(c, err)
	}
	return c.JSON(publicUser(updated))
}

func publicUser(u user.User) user.User {
	u.ProfilePictureURL = media.PublicURL("", u.ProfilePictureURL)
	u.StudentCardLinked = u.StudentCardUID != ""
	return u
}
