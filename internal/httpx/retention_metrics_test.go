package httpx_test

import (
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
)

// The retention sweep's metrics are served beside the others while its mode
// is on, and not at all while it is off.
func TestMetricsPublishTheRetentionMetrics(t *testing.T) {
	t.Parallel()

	metrics := fixedGauges("skylab_retention_attention 0\n")
	status, text := scrapeMetrics(t, httpx.Deps{RetentionMetrics: metrics})
	if status != fiber.StatusOK || text != string(metrics) {
		t.Fatalf("retention-only metrics status=%d body=\n%s", status, text)
	}

	status, text = scrapeMetrics(t, httpx.Deps{AccountAccessMetrics: accessgate.NewMetrics(), RetentionMetrics: metrics})
	if status != fiber.StatusOK || !strings.HasSuffix(text, string(metrics)) || !strings.Contains(text, "skylab_account_access_") {
		t.Fatalf("metrics status=%d body=\n%s", status, text)
	}

	status, text = scrapeMetrics(t, httpx.Deps{AccountAccessMetrics: accessgate.NewMetrics()})
	if status != fiber.StatusOK || strings.Contains(text, "skylab_retention_") {
		t.Fatalf("sweep-off metrics status=%d body=\n%s", status, text)
	}
}
