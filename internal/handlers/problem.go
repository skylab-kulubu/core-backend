package handlers

import (
	"encoding/json"
	"errors"
	"log"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func lifecycleVisibility(c fiber.Ctx) (lifecycle.Visibility, error) {
	return lifecycle.ParseVisibility(c.Query("lifecycle"))
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
	body := fiber.Map{
		"type":     "about:blank",
		"title":    title,
		"status":   status,
		"detail":   detail,
		"instance": instance,
	}
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
	log.Printf("http error: %v", err)
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return problem(c, fe.Code, fe.Message)
	}
	if errors.Is(err, user.ErrConflict) {
		return problem(c, fiber.StatusConflict, "Conflict")
	}
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}
