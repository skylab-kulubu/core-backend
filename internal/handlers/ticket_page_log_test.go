package handlers

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
)

func TestLoggedPathCutsTheTicketKey(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"/v1/public/tickets/ABCDEFGHIJKLMNOPQRSTUVWXYZ":     "/v1/public/tickets/ABCD…",
		"/v1/public/tickets/ABCDEFGHIJKLMNOPQRSTUVWXYZ/pdf": "/v1/public/tickets/ABCD…/pdf",
		"/v1/public/tickets/ABCDEFGHIJKLMNOPQRSTUVWXYZ/x/y": "/v1/public/tickets/ABCD…/x/y",
		"/v1/public/tickets/":                               "/v1/public/tickets/…",
		"/V1/PUBLIC/TICKETS/ABCDEFGHIJKLMNOPQRSTUVWXYZ/pdf": "/V1/PUBLIC/TICKETS/ABCD…/pdf",
		"/v1/Public/Tickets/ABCDEFGHIJKLMNOPQRSTUVWXYZ":     "/v1/Public/Tickets/ABCD…",
		"/v1/public/ticket":                                 "/v1/public/ticket",
		"/v1/tickets/3f1c2b6e-1111-4a2b-8c3d-000000000001":  "/v1/tickets/3f1c2b6e-1111-4a2b-8c3d-000000000001",
		"/v1/certificates/verify/SERIAL":                    "/v1/certificates/verify/SERIAL",
	} {
		if got := loggedPath(in); got != want {
			t.Errorf("loggedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The error handler never writes a ticket page's key: not for a route that
// does not exist under it, not for a wrong method, not for a panic.
func TestErrorHandlerLogNeverHoldsTheTicketKey(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(io.MultiWriter(&logs, previous))
	t.Cleanup(func() { log.SetOutput(previous) })
	const key = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(recover.New())
	app.Get("/v1/public/tickets/:key", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	app.Get("/v1/public/tickets/:key/pdf", func(c fiber.Ctx) error { panic(errors.New("boom")) })
	for _, tc := range []struct{ method, path string }{
		{fiber.MethodGet, "/v1/public/tickets/" + key + "/nope"},
		{fiber.MethodPost, "/v1/public/tickets/" + key},
		{fiber.MethodGet, "/v1/public/tickets/" + key + "/pdf"},
		// The router matches in any case: so does the cut.
		{fiber.MethodGet, "/V1/PUBLIC/TICKETS/" + key + "/PDF"},
		{fiber.MethodGet, "/V1/Public/Tickets/" + key + "/nope"},
	} {
		if _, err := app.Test(httptest.NewRequest(tc.method, tc.path, nil)); err != nil {
			t.Fatal(err)
		}
	}
	text := logs.String()
	if strings.Contains(text, key) || strings.Count(strings.ToLower(text), "/v1/public/tickets/abcd…") < 5 {
		t.Fatalf("log:\n%s", text)
	}
}
