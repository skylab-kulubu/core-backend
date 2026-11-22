package media

import (
	"strings"
	"testing"
)

// The worker's counters add up each pass's outcomes and count the passes
// the source stopped.
func TestMapWorkerCounters(t *testing.T) {
	t.Parallel()
	w := &MapWorker{}
	w.count(MapReport{Made: 2, Retried: 1, Abandoned: 1}, false)
	w.count(MapReport{Made: 1, NoMap: 1}, true)
	text := w.Prometheus()
	for _, want := range []string{
		"# TYPE skylab_event_maps_total counter",
		`skylab_event_maps_total{outcome="made"} 3`, `skylab_event_maps_total{outcome="retried"} 1`,
		`skylab_event_maps_total{outcome="given_up"} 1`, `skylab_event_maps_total{outcome="no_map"} 1`,
		`skylab_event_maps_total{outcome="source_down"} 1`, `skylab_event_maps_total{outcome="discarded"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in %s", want, text)
		}
	}
	var none *MapWorker
	if none.Prometheus() != "" {
		t.Error("a worker that is off has counters")
	}
}
