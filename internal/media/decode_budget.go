package media

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrDecodeBusy refuses work that waited its whole turn for a decoding slot:
// other images are being decoded. Nothing was stored; the same request can
// be retried.
var ErrDecodeBusy = errors.New("media: busy decoding other images")

// The decode budget's settings (docs/media-lifecycle.md, "Decode budget").
const (
	// DecodeSlotsEnv is how many images core decodes at once, 1 to
	// maxDecodeSlots; unset is 2.
	DecodeSlotsEnv = "MEDIA_DECODE_SLOTS"
	// DecodeWaitEnv is how long a request waits for a slot before
	// media_busy, a Go duration up to maxDecodeWait; unset is 10s.
	DecodeWaitEnv = "MEDIA_DECODE_WAIT"

	defaultDecodeSlots = 2
	defaultDecodeWait  = 10 * time.Second
	maxDecodeSlots     = 8
	maxDecodeWait      = time.Minute
)

// DecodeBudget bounds the images core decodes at once, across everything
// that decodes one: uploads for a purpose, SVG rasterizing, cover colours
// and the backfills. A process has one, made at startup and handed to each
// of them, so that together they hold at most Slots decoded images in
// memory (docs/media-lifecycle.md, "Decode budget", has the worst case).
// It counts its requests for /v1/metrics.
type DecodeBudget struct {
	slots chan struct{}
	svg   chan struct{}
	wait  time.Duration

	waiting                                     atomic.Int64
	immediate, waited, busy, cancelled, skipped atomic.Int64
	waitNanos                                   atomic.Int64
}

// DecodeBudgetConfig sizes a DecodeBudget. Zero fields take the defaults.
type DecodeBudgetConfig struct {
	// Slots is how many images may be decoded at once; default 2.
	Slots int
	// Wait is how long work waits for a slot before ErrDecodeBusy; default
	// 10 seconds.
	Wait time.Duration
}

// DecodeBudgetConfigFromEnv reads MEDIA_DECODE_SLOTS and MEDIA_DECODE_WAIT;
// each one unset keeps its default. A value core cannot use is an error:
// a budget silently wider than the memory limit was sized for would end in
// the container being killed.
func DecodeBudgetConfigFromEnv(getenv func(string) string) (DecodeBudgetConfig, error) {
	var config DecodeBudgetConfig
	if raw := strings.TrimSpace(getenv(DecodeSlotsEnv)); raw != "" {
		slots, err := strconv.Atoi(raw)
		if err != nil || slots < 1 || slots > maxDecodeSlots {
			return DecodeBudgetConfig{}, fmt.Errorf("%s must be an integer from 1 to %d", DecodeSlotsEnv, maxDecodeSlots)
		}
		config.Slots = slots
	}
	if raw := strings.TrimSpace(getenv(DecodeWaitEnv)); raw != "" {
		wait, err := time.ParseDuration(raw)
		if err != nil || wait <= 0 || wait > maxDecodeWait {
			return DecodeBudgetConfig{}, fmt.Errorf("%s must be a positive duration of at most %s (such as 10s)", DecodeWaitEnv, maxDecodeWait)
		}
		config.Wait = wait
	}
	return config, nil
}

// NewDecodeBudget makes a budget. SVG gets one slot of its own on top of
// the shared ones: at most one SVG is sanitized at a time.
func NewDecodeBudget(config DecodeBudgetConfig) *DecodeBudget {
	if config.Slots <= 0 {
		config.Slots = defaultDecodeSlots
	}
	if config.Wait <= 0 {
		config.Wait = defaultDecodeWait
	}
	return &DecodeBudget{
		slots: make(chan struct{}, config.Slots),
		svg:   make(chan struct{}, 1),
		wait:  config.Wait,
	}
}

// String describes the budget for the startup log.
func (b *DecodeBudget) String() string {
	return fmt.Sprintf("%d images at once, 1 of them an SVG; a request waits at most %s", cap(b.slots), b.wait)
}

// Acquire waits for a decoding slot, at most the budget's wait and while
// ctx lasts: ErrDecodeBusy when the wait runs out, ctx's error when ctx
// ends first. release ends the turn.
func (b *DecodeBudget) Acquire(ctx context.Context) (release func(), err error) {
	return b.take(ctx, b.slots)
}

// TryAcquire takes a decoding slot only when one is free now, for work
// that is better skipped than waited for.
func (b *DecodeBudget) TryAcquire() (release func(), ok bool) {
	select {
	case b.slots <- struct{}{}:
		b.immediate.Add(1)
		return func() { <-b.slots }, true
	default:
		b.skipped.Add(1)
		return nil, false
	}
}

// AcquireSVG waits, within one wait, for the SVG slot (one SVG is
// sanitized at a time) and then a shared slot.
func (b *DecodeBudget) AcquireSVG(ctx context.Context) (release func(), err error) {
	return b.take(ctx, b.svg, b.slots)
}

// take takes a slot of each of pools, in order, within one wait, and
// counts the request once: immediate when every slot was free, else
// waited, busy or cancelled.
func (b *DecodeBudget) take(ctx context.Context, pools ...chan struct{}) (func(), error) {
	if release, ok := tryTake(pools); ok {
		b.immediate.Add(1)
		return release, nil
	}
	b.waiting.Add(1)
	start := time.Now()
	timer := time.NewTimer(b.wait)
	defer timer.Stop()
	release, err := b.takeWaiting(ctx, timer.C, pools)
	b.waiting.Add(-1)
	b.waitNanos.Add(int64(time.Since(start)))
	switch {
	case err == nil:
		b.waited.Add(1)
	case errors.Is(err, ErrDecodeBusy):
		b.busy.Add(1)
	default:
		b.cancelled.Add(1)
	}
	return release, err
}

// tryTake takes a slot of each pool only when all are free now.
func tryTake(pools []chan struct{}) (func(), bool) {
	for i, pool := range pools {
		select {
		case pool <- struct{}{}:
		default:
			releaseAll(pools[:i])()
			return nil, false
		}
	}
	return releaseAll(pools), true
}

func (b *DecodeBudget) takeWaiting(ctx context.Context, expired <-chan time.Time, pools []chan struct{}) (func(), error) {
	for i, pool := range pools {
		select {
		case pool <- struct{}{}:
		case <-expired:
			releaseAll(pools[:i])()
			return nil, ErrDecodeBusy
		case <-ctx.Done():
			releaseAll(pools[:i])()
			return nil, ctx.Err()
		}
	}
	return releaseAll(pools), nil
}

// releaseAll gives back a slot of each pool, the last taken first.
func releaseAll(pools []chan struct{}) func() {
	return func() {
		for i := len(pools) - 1; i >= 0; i-- {
			<-pools[i]
		}
	}
}

// Prometheus is the budget's state and counters in Prometheus' text
// format: how full it is, how many requests wait now, and how its requests
// ended (immediate, waited, busy: the whole wait ran out, cancelled: the
// work's context ended while waiting, skipped: cover colours that did not
// wait).
// Nil renders nothing.
func (b *DecodeBudget) Prometheus() string {
	if b == nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# TYPE skylab_media_decode_slots gauge\nskylab_media_decode_slots %d\n", cap(b.slots))
	fmt.Fprintf(&out, "# TYPE skylab_media_decode_slots_in_use gauge\nskylab_media_decode_slots_in_use %d\n", len(b.slots))
	fmt.Fprintf(&out, "# TYPE skylab_media_decode_waiting gauge\nskylab_media_decode_waiting %d\n", b.waiting.Load())
	out.WriteString("# TYPE skylab_media_decode_requests_total counter\n")
	for _, outcome := range []struct {
		name  string
		value int64
	}{
		{"immediate", b.immediate.Load()}, {"waited", b.waited.Load()}, {"busy", b.busy.Load()},
		{"cancelled", b.cancelled.Load()}, {"skipped", b.skipped.Load()},
	} {
		fmt.Fprintf(&out, "skylab_media_decode_requests_total{outcome=%q} %d\n", outcome.name, outcome.value)
	}
	fmt.Fprintf(&out, "# TYPE skylab_media_decode_wait_seconds_total counter\nskylab_media_decode_wait_seconds_total %s\n",
		strconv.FormatFloat(time.Duration(b.waitNanos.Load()).Seconds(), 'f', -1, 64))
	return out.String()
}
