package handlers

import (
	"encoding/json"
	"errors"
	"log"

	"github.com/gofiber/fiber/v3"
)

func problem(c fiber.Ctx, status int, title string) error {
	return problemDetail(c, status, title, title)
}

func problemDetail(c fiber.Ctx, status int, title, detail string) error {
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
	payload, err := json.Marshal(fiber.Map{
		"type":     "about:blank",
		"title":    title,
		"status":   status,
		"detail":   detail,
		"instance": instance,
	})
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
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}
