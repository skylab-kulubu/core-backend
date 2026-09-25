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
	if handled, problemErr := purposeProblem(c, err); handled {
		return problemErr
	}
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, user.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, user.ErrInvalid), errors.Is(err, media.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	case errors.Is(err, user.ErrYTUManaged):
		return problemCode(c, fiber.StatusConflict, "Conflict", ytuManagedCode)
	case errors.Is(err, media.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, user.ErrAccountBlocked):
		// /me is the caller's own account: a blocked subject is refused the
		// same way the JIT guard and short-link attribution refuse its token.
		c.Set(fiber.HeaderCacheControl, "no-store")
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	default:
		return err
	}
}

// meUser is the caller's own view of their User shadow. It is the only
// payload that carries phone (read-only in Account center until phone
// verification exists); the domain struct keeps `json:"-"` so admin cards,
// rosters, search results, SkyPass and ticket payloads never pick it up.
//
// ytuLinked tells the caller that university, faculty and department follow
// the YTÜ Microsoft login and are read-only (an edit that changes them is
// refused with 409 `ytu_managed_field`). It is always present so a client
// never mistakes an older core for a non-YTÜ person.
type meUser struct {
	user.User
	Phone     string `json:"phone,omitempty"`
	YTULinked bool   `json:"ytuLinked"`
}

// ytuManagedCode is the problem code for an edit that would change a
// university, faculty or department that follows the YTÜ login.
const ytuManagedCode = "ytu_managed_field"

func (h *MeHandler) GetMe(c fiber.Ctx) error {
	u, ok := c.Locals(authn.LocalsUser).(user.User)
	if !ok {
		return fiber.ErrUnauthorized
	}
	return c.JSON(meView(u))
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
	return c.JSON(meView(updated))
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
	return c.JSON(meView(updated))
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
	uploaded, err := h.media.UploadForPurpose(c.Context(), p, media.PurposeProfilePicture, media.UploadedFile{
		Name: header.Filename, ContentType: header.Header.Get("Content-Type"), Data: data,
	})
	if err != nil {
		return meError(c, err)
	}
	updated, err := h.users.SetProfilePicture(c.Context(), u.ID, uploaded.ID, uploaded.URL)
	if err != nil {
		return meError(c, err)
	}
	return c.JSON(meView(updated))
}

// DeleteProfilePicture removes the caller's own profile picture. It is an
// idempotent lifecycle transition, not a physical delete: the person's upload
// is archived and the shadow drops the link, so a repeated call is still 204.
//
// The archive happens first so a partial failure converges on retry: an
// archived-but-still-linked picture is cleared by the next call, whereas an
// unlinked-but-never-archived upload would be invisible to it.
func (h *MeHandler) DeleteProfilePicture(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return meError(c, err)
	}
	u, err := h.identityID(c)
	if err != nil {
		return meError(c, err)
	}
	// The archive is the first side effect and the media store does not know
	// the account state, so refuse a blocked account before touching it; the
	// user store repeats the rule on the unlink like every other self-write.
	if u.AccountState != user.AccountActive {
		return meError(c, user.ErrAccountBlocked)
	}
	if u.ProfilePictureID != nil {
		// Only the person's own upload is archived. A picture that is not
		// theirs, or is already gone, stays with its uploader; the profile no
		// longer links it either way.
		archiveErr := h.media.ArchiveOwn(c.Context(), p, *u.ProfilePictureID)
		if archiveErr != nil && !errors.Is(archiveErr, media.ErrNotFound) && !errors.Is(archiveErr, media.ErrForbidden) {
			return meError(c, archiveErr)
		}
	}
	if _, err := h.users.ClearProfilePicture(c.Context(), u.ID); err != nil {
		return meError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func meView(u user.User) meUser {
	u.ProfilePictureURL = media.PublicURL("", u.ProfilePictureURL)
	u.StudentCardLinked = u.StudentCardUID != ""
	return meUser{User: u, Phone: u.Phone, YTULinked: u.YTULinked}
}
