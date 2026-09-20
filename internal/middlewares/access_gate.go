package middlewares

import (
	"encoding/json"
	"log"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

func AccountAccessGate(reader accessgate.Reader, metricSets ...*accessgate.Metrics) fiber.Handler {
	var metrics *accessgate.Metrics
	if len(metricSets) > 0 {
		metrics = metricSets[0]
	}
	return accountAccessGate(reader, metrics, log.Default())
}

func accountAccessGate(reader accessgate.Reader, metrics *accessgate.Metrics, logger *log.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		identity, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
		if !ok {
			return c.Next()
		}
		if reader == nil {
			return c.Next()
		}
		c.Set(fiber.HeaderCacheControl, "no-store")
		decision := reader.Check(c.Context(), identity.ID.String())
		metrics.RecordDecision(decision)
		logAccessDecision(logger, c, decision)
		switch decision {
		case accessgate.Allowed:
			c.Response().Header.Del(fiber.HeaderCacheControl)
			return c.Next()
		case accessgate.Blocked:
			c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
			return fiber.ErrUnauthorized
		default:
			c.Set(fiber.HeaderRetryAfter, "1")
			return fiber.ErrServiceUnavailable
		}
	}
}

func logAccessDecision(logger *log.Logger, c fiber.Ctx, decision accessgate.Decision) {
	if logger == nil {
		return
	}
	payload, err := json.Marshal(struct {
		Event         string `json:"event"`
		CorrelationID string `json:"correlation_id"`
		Decision      string `json:"decision"`
	}{
		Event:         "account_access_gate",
		CorrelationID: requestid.FromContext(c),
		Decision:      string(decision),
	})
	if err == nil {
		logger.Print(string(payload))
	}
}
