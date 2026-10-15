package media_test

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

func TestDecodeBudgetConfigFromEnv(t *testing.T) {
	t.Parallel()
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	config, err := media.DecodeBudgetConfigFromEnv(env(nil))
	if err != nil || config != (media.DecodeBudgetConfig{}) {
		t.Fatalf("unset: %+v, %v; want the defaults", config, err)
	}
	config, err = media.DecodeBudgetConfigFromEnv(env(map[string]string{"MEDIA_DECODE_SLOTS": " 1 ", "MEDIA_DECODE_WAIT": "30s"}))
	if err != nil || config.Slots != 1 || config.Wait != 30*time.Second {
		t.Fatalf("set: %+v, %v", config, err)
	}
	for _, bad := range []map[string]string{
		{"MEDIA_DECODE_SLOTS": "0"},
		{"MEDIA_DECODE_SLOTS": "9"},
		{"MEDIA_DECODE_SLOTS": "two"},
		{"MEDIA_DECODE_WAIT": "0s"},
		{"MEDIA_DECODE_WAIT": "2m"},
		{"MEDIA_DECODE_WAIT": "10"},
	} {
		if _, err := media.DecodeBudgetConfigFromEnv(env(bad)); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
}

func TestDecodeBudgetDescribesItself(t *testing.T) {
	t.Parallel()
	budget := media.NewDecodeBudget(media.DecodeBudgetConfig{})
	if got := budget.String(); got != "2 images at once, 1 of them an SVG; a request waits at most 10s" {
		t.Fatalf("String() = %q", got)
	}
}

// metric reads one sample of the budget's metrics; -1 when it is missing.
func metric(t *testing.T, text, sample string) float64 {
	t.Helper()
	match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(sample) + ` (\S+)$`).FindStringSubmatch(text)
	if match == nil {
		return -1
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		t.Fatalf("%s: %v", sample, err)
	}
	return value
}

func TestDecodeBudgetMetricsShowWaitsAndSaturation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	budget := media.NewDecodeBudget(media.DecodeBudgetConfig{Slots: 1, Wait: 50 * time.Millisecond})

	text := budget.Prometheus()
	for sample, want := range map[string]float64{
		"skylab_media_decode_slots":                               1,
		"skylab_media_decode_slots_in_use":                        0,
		"skylab_media_decode_waiting":                             0,
		`skylab_media_decode_requests_total{outcome="immediate"}`: 0,
		`skylab_media_decode_requests_total{outcome="waited"}`:    0,
		`skylab_media_decode_requests_total{outcome="busy"}`:      0,
		`skylab_media_decode_requests_total{outcome="cancelled"}`: 0,
		`skylab_media_decode_requests_total{outcome="skipped"}`:   0,
		"skylab_media_decode_wait_seconds_total":                  0,
	} {
		if got := metric(t, text, sample); got != want {
			t.Errorf("before: %s = %v, want %v", sample, got, want)
		}
	}
	for _, line := range []string{
		"# TYPE skylab_media_decode_slots gauge\n",
		"# TYPE skylab_media_decode_requests_total counter\n",
		"# TYPE skylab_media_decode_wait_seconds_total counter\n",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("no %q in\n%s", line, text)
		}
	}

	release, err := budget.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := metric(t, budget.Prometheus(), "skylab_media_decode_slots_in_use"); got != 1 {
		t.Fatalf("in use = %v while held", got)
	}
	if _, ok := budget.TryAcquire(); ok {
		t.Fatal("TryAcquire took a held slot")
	}

	// One request waits behind the held slot and is refused busy; while it
	// waits it shows as waiting.
	refused := make(chan error, 1)
	go func() {
		_, err := budget.Acquire(ctx)
		refused <- err
	}()
	deadline := time.Now().Add(time.Second)
	for metric(t, budget.Prometheus(), "skylab_media_decode_waiting") != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the second request never showed as waiting")
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-refused; !errors.Is(err, media.ErrDecodeBusy) {
		t.Fatalf("second request: %v, want %v", err, media.ErrDecodeBusy)
	}

	// Work whose context ended is counted apart from busy.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := budget.Acquire(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}

	// One that waits and gets the slot.
	got := make(chan func(), 1)
	go func() {
		release, err := budget.Acquire(ctx)
		if err != nil {
			t.Error(err)
		}
		got <- release
	}()
	for metric(t, budget.Prometheus(), "skylab_media_decode_waiting") != 1 {
		time.Sleep(time.Millisecond)
	}
	release()
	(<-got)()

	text = budget.Prometheus()
	for sample, want := range map[string]float64{
		"skylab_media_decode_slots_in_use":                        0,
		"skylab_media_decode_waiting":                             0,
		`skylab_media_decode_requests_total{outcome="immediate"}`: 1,
		`skylab_media_decode_requests_total{outcome="waited"}`:    1,
		`skylab_media_decode_requests_total{outcome="busy"}`:      1,
		`skylab_media_decode_requests_total{outcome="cancelled"}`: 1,
		`skylab_media_decode_requests_total{outcome="skipped"}`:   1,
	} {
		if got := metric(t, text, sample); got != want {
			t.Errorf("after: %s = %v, want %v", sample, got, want)
		}
	}
	if waited := metric(t, text, "skylab_media_decode_wait_seconds_total"); waited < 0.05 {
		t.Errorf("wait seconds = %v, want at least the busy request's 50ms", waited)
	}
}

func TestDecodeBudgetCountsAnSVGOnce(t *testing.T) {
	t.Parallel()
	budget := media.NewDecodeBudget(media.DecodeBudgetConfig{Slots: 2, Wait: 20 * time.Millisecond})
	release, err := budget.AcquireSVG(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := budget.Prometheus()
	if got := metric(t, text, `skylab_media_decode_requests_total{outcome="immediate"}`); got != 1 {
		t.Fatalf("immediate = %v, want 1 for one SVG", got)
	}
	if got := metric(t, text, "skylab_media_decode_slots_in_use"); got != 1 {
		t.Fatalf("in use = %v, want the one shared slot the SVG holds", got)
	}
	release()
	if got := metric(t, budget.Prometheus(), "skylab_media_decode_slots_in_use"); got != 0 {
		t.Fatalf("in use = %v after release", got)
	}
}

func TestNilDecodeBudgetHasNoMetrics(t *testing.T) {
	t.Parallel()
	var budget *media.DecodeBudget
	if got := budget.Prometheus(); got != "" {
		t.Fatalf("nil budget: %q", got)
	}
}
