package httpx_test

import (
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
)

type fixedUserSync string

func (fixedUserSync) Wake()                {}
func (s fixedUserSync) Prometheus() string { return string(s) }

func TestMetricsPublishTheUserSyncCounters(t *testing.T) {
	t.Parallel()

	counters := fixedUserSync("skylab_user_sync_enabled 1\n")
	status, text := scrapeMetrics(t, httpx.Deps{UserSync: counters})
	if status != fiber.StatusOK || text != string(counters) {
		t.Fatalf("user sync metrics status=%d body=\n%s", status, text)
	}
}
