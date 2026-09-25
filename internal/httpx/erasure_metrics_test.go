package httpx_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
)

type fixedGauges string

func (g fixedGauges) Prometheus() string { return string(g) }

func scrapeMetrics(t *testing.T, deps httpx.Deps) (int, string) {
	t.Helper()
	response, err := httpx.New(deps).Test(httptest.NewRequest(fiber.MethodGet, "/v1/metrics", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func TestMetricsPublishTheAccountErasureGaugesBesideTheAccessCounters(t *testing.T) {
	t.Parallel()

	gauges := fixedGauges("skylab_account_erasure_open_requests 2\nskylab_account_erasure_overdue_requests 1\n")
	status, text := scrapeMetrics(t, httpx.Deps{AccountAccessMetrics: accessgate.NewMetrics(), AccountErasureMetrics: gauges})
	if status != fiber.StatusOK || !strings.Contains(text, "skylab_account_access_decisions_allowed_total 0\n") ||
		!strings.HasSuffix(text, string(gauges)) {
		t.Fatalf("metrics status=%d body=\n%s", status, text)
	}

	status, text = scrapeMetrics(t, httpx.Deps{AccountErasureMetrics: gauges})
	if status != fiber.StatusOK || text != string(gauges) {
		t.Fatalf("erasure-only metrics status=%d body=\n%s", status, text)
	}

	status, text = scrapeMetrics(t, httpx.Deps{AccountAccessMetrics: accessgate.NewMetrics()})
	if status != fiber.StatusOK || strings.Contains(text, "skylab_account_erasure_") {
		t.Fatalf("worker-off metrics status=%d body=\n%s", status, text)
	}
}
