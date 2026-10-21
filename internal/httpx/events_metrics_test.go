package httpx_test

import (
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
)

type fixedMetrics string

func (m fixedMetrics) Prometheus() string { return string(m) }

func TestMetricsPublishTheEventsCounters(t *testing.T) {
	t.Parallel()

	counters := fixedMetrics("skylab_events_enabled 1\nskylab_events_connected 0\n")
	status, text := scrapeMetrics(t, httpx.Deps{EventsMetrics: counters})
	if status != fiber.StatusOK || text != string(counters) {
		t.Fatalf("events metrics status=%d body=\n%s", status, text)
	}
}

// EVENTS_ENABLED off: the consumer is nil and says so.
func TestMetricsPublishEventsOff(t *testing.T) {
	t.Parallel()

	var off *events.Consumer
	status, text := scrapeMetrics(t, httpx.Deps{EventsMetrics: off})
	if status != fiber.StatusOK || text != "# TYPE skylab_events_enabled gauge\nskylab_events_enabled 0\n" {
		t.Fatalf("events metrics status=%d body=\n%s", status, text)
	}
}
