package handlers

import "github.com/gofiber/fiber/v3"

// The request's own shape, refused before any service reads it
// (docs/problem-codes.md): which path segment, the body or which query
// parameter could not be read. The status and the title stay 400 "Bad
// Request"; the code says why and param names the part.

// badID answers a path segment that is not an id.
func badID(c fiber.Ctx, param string) error {
	return problemWithFields(c, fiber.StatusBadRequest, "Bad Request", "Bad Request", "invalid_id", fiber.Map{"param": param})
}

// badBody answers a body that is not the route's JSON.
func badBody(c fiber.Ctx) error {
	return problemCode(c, fiber.StatusBadRequest, "Bad Request", "invalid_body")
}

// badQuery answers a query parameter the route cannot take.
func badQuery(c fiber.Ctx, param string) error {
	return problemWithFields(c, fiber.StatusBadRequest, "Bad Request", "Bad Request", "invalid_query", fiber.Map{"param": param})
}
