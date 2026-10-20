package handlers

import (
	"encoding/json"
	"errors"
	"log"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func lifecycleVisibility(c fiber.Ctx) (lifecycle.Visibility, error) {
	return lifecycle.ParseVisibility(c.Query("lifecycle"))
}

// eventStatusCode names why an Event's status refused the request (ADR
// 0066): event_not_published for a draft, event_cancelled for a cancelled
// Event, event_has_tickets or event_has_competitors for one that cannot
// become a draft again, "" for anything else.
func eventStatusCode(err error) string {
	switch {
	case errors.Is(err, event.ErrNotPublished):
		return "event_not_published"
	case errors.Is(err, event.ErrCancelled):
		return "event_cancelled"
	case errors.Is(err, event.ErrHasTickets):
		return "event_has_tickets"
	case errors.Is(err, event.ErrHasCompetitors):
		return "event_has_competitors"
	case errors.Is(err, event.ErrNotDraft):
		return "event_not_draft"
	default:
		return ""
	}
}

func problem(c fiber.Ctx, status int, title string) error {
	return problemDetail(c, status, title, title)
}

func problemDetail(c fiber.Ctx, status int, title, detail string) error {
	return problemDetailCode(c, status, title, detail, "")
}

func problemCode(c fiber.Ctx, status int, title, code string) error {
	return problemDetailCode(c, status, title, title, code)
}

func problemDetailCode(c fiber.Ctx, status int, title, detail, code string) error {
	return problemWithFields(c, status, title, detail, code, nil)
}

// problemWithFields adds extension members (RFC 9457 §3.2) that tell the
// caller how to fix the request. They never replace the standard members.
func problemWithFields(c fiber.Ctx, status int, title, detail, code string, fields fiber.Map) error {
	instance := c.Path()
	if u := c.Request().URI(); u != nil {
		path := string(u.Path())
		if path != "" {
			instance = path
			if q := u.QueryString(); len(q) > 0 {
				instance += "?" + string(q)
			}
		}
	}
	body := fiber.Map{}
	for name, value := range fields {
		body[name] = value
	}
	body["type"] = "about:blank"
	body["title"] = title
	body["status"] = status
	body["detail"] = detail
	body["instance"] = instance
	if code != "" {
		body["code"] = code
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, "application/problem+json")
	return c.Status(status).Send(payload)
}

func ErrorHandler(c fiber.Ctx, err error) error {
	// Name the route so an unmatched path or wrong method is traceable to its
	// caller. The query string stays out: it can carry share tokens.
	log.Printf("http error: %s %s: %v", c.Method(), c.Path(), err)
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return problem(c, fe.Code, fe.Message)
	}
	if errors.Is(err, user.ErrConflict) {
		return problem(c, fiber.StatusConflict, "Conflict")
	}
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}
