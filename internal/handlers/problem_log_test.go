package handlers

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// Not parallel: it captures the process-wide logger, and parallel tests in
// this package run only after it has restored it.
func TestErrorHandlerLogNamesTheRequestWithoutItsQuery(t *testing.T) {
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	defer log.SetOutput(previous)

	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/v1/known", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	for _, tc := range []struct {
		method, target string
		status         int
		want           string
	}{
		{fiber.MethodGet, "/api/media?token=secret", fiber.StatusNotFound, "http error: GET /api/media: Not Found"},
		{fiber.MethodPost, "/v1/known", fiber.StatusMethodNotAllowed, "http error: POST /v1/known: Method Not Allowed"},
	} {
		captured.Reset()
		resp, err := app.Test(httptest.NewRequest(tc.method, tc.target, nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.status {
			t.Fatalf("%s %s status = %d, want %d", tc.method, tc.target, resp.StatusCode, tc.status)
		}
		line := captured.String()
		if !strings.Contains(line, tc.want) {
			t.Fatalf("log = %q, want it to contain %q", line, tc.want)
		}
		if strings.Contains(line, "secret") {
			t.Fatalf("log = %q leaks the query string", line)
		}
	}
}
