package middlewares_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
)

type gateReader struct {
	decision accessgate.Decision
	calls    int
}

func (g *gateReader) Check(context.Context, string) accessgate.Decision {
	g.calls++
	return g.decision
}

func (*gateReader) Ready(context.Context) error { return nil }

func TestAccountAccessGateSkipsAnonymousAndAllowsOnlyExplicitAllowedDecision(t *testing.T) {
	t.Parallel()

	reader := &gateReader{decision: accessgate.Allowed}
	app := fiber.New()
	app.Use(middlewares.AccountAccessGate(reader))
	handled := 0
	app.Get("/", func(c fiber.Ctx) error { handled++; return c.SendStatus(fiber.StatusNoContent) })

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	if err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("anonymous response=%v err=%v", response, err)
	}
	if reader.calls != 0 || handled != 1 {
		t.Fatalf("anonymous calls=%d handled=%d", reader.calls, handled)
	}

	app = fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111")})
		return c.Next()
	})
	app.Use(middlewares.AccountAccessGate(reader))
	app.Get("/", func(c fiber.Ctx) error { handled++; return c.SendStatus(fiber.StatusNoContent) })
	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	if err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("allowed response=%v err=%v", response, err)
	}
	if reader.calls != 1 || handled != 2 {
		t.Fatalf("allowed calls=%d handled=%d", reader.calls, handled)
	}
}

func TestAccountAccessGateDoesNotTreatAnAuthenticatedNilUUIDAsAnonymous(t *testing.T) {
	t.Parallel()

	reader := &gateReader{decision: accessgate.Blocked}
	handled := false
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.Nil})
		return c.Next()
	})
	app.Use(middlewares.AccountAccessGate(reader))
	app.Get("/", func(c fiber.Ctx) error { handled = true; return c.SendStatus(fiber.StatusNoContent) })

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusUnauthorized || reader.calls != 1 || handled {
		t.Fatalf("status=%d calls=%d handled=%v", response.StatusCode, reader.calls, handled)
	}
}

func TestAccountAccessGateStopsBlockedAndUnavailableRequestsBeforeHandlers(t *testing.T) {
	t.Parallel()

	const rawSubject = "abcdefab-1111-1111-1111-111111111111"
	for _, test := range []struct {
		name       string
		decision   accessgate.Decision
		status     int
		retryAfter string
	}{
		{name: "blocked", decision: accessgate.Blocked, status: fiber.StatusUnauthorized},
		{name: "unavailable", decision: accessgate.Unavailable, status: fiber.StatusServiceUnavailable, retryAfter: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &gateReader{decision: test.decision}
			handled := false
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.MustParse(rawSubject)})
				return c.Next()
			})
			app.Use(middlewares.AccountAccessGate(reader))
			app.Get("/", func(c fiber.Ctx) error { handled = true; return c.SendStatus(fiber.StatusNoContent) })

			response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != test.status || handled {
				t.Fatalf("status=%d handled=%v body=%s", response.StatusCode, handled, body)
			}
			if response.Header.Get(fiber.HeaderCacheControl) != "no-store" || response.Header.Get(fiber.HeaderRetryAfter) != test.retryAfter {
				t.Fatalf("headers = %v", response.Header)
			}
			if strings.Contains(string(body), rawSubject) {
				t.Fatal("response exposed the raw subject")
			}
			if test.decision == accessgate.Blocked && response.Header.Get(fiber.HeaderWWWAuthenticate) != `Bearer error="invalid_token"` {
				t.Fatalf("WWW-Authenticate = %q", response.Header.Get(fiber.HeaderWWWAuthenticate))
			}
		})
	}
}
