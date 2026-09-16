package handlers

import "github.com/gofiber/fiber/v3"

func problem(c fiber.Ctx, status int, title string) error {
	err := c.Status(status).JSON(fiber.Map{
		"type":   "about:blank",
		"title":  title,
		"status": status,
	})
	c.Set("Content-Type", "application/problem+json")
	return err
}
