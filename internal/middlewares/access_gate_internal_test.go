package middlewares

import (
	"bytes"
	"context"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

type loggingGate struct{ decision accessgate.Decision }

func (g loggingGate) Check(context.Context, string) accessgate.Decision { return g.decision }
func (loggingGate) Ready(context.Context) error                         { return nil }

func TestAccessGateLogsDecisionWithCorrelationIDWithoutAccountMaterial(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger := log.New(&output, "", 0)
	subjectID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	app := fiber.New()
	app.Use(requestid.New())
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: subjectID})
		return c.Next()
	})
	app.Use(accountAccessGate(loggingGate{decision: accessgate.Blocked}, nil, logger))
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	request := httptest.NewRequest(fiber.MethodGet, "/", nil)
	request.Header.Set(fiber.HeaderXRequestID, "correlation-123")
	response, err := app.Test(request)
	if err != nil || response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("response=%v err=%v", response, err)
	}
	got := output.String()
	for _, want := range []string{
		`"event":"account_access_gate"`,
		`"correlation_id":"correlation-123"`,
		`"decision":"blocked"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log missing %q: %s", want, got)
		}
	}
	digest := strings.TrimPrefix(accessgate.MarkerKey(subjectID.String()), accessgate.MarkerKeyPrefix)
	if strings.Contains(got, subjectID.String()) || strings.Contains(got, digest) || strings.Contains(got, accessgate.MarkerKey(subjectID.String())) {
		t.Fatalf("log exposed account material: %s", got)
	}
}
