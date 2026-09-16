package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
)

func problem(c fiber.Ctx, status int, title string) error {
	err := c.Status(status).JSON(fiber.Map{
		"type":   "about:blank",
		"title":  title,
		"status": status,
	})
	c.Set("Content-Type", "application/problem+json")
	return err
}

func ErrorHandler(c fiber.Ctx, err error) error {
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return problem(c, fe.Code, fe.Message)
	}
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}
