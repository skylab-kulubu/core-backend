package media

import (
	"context"
	"errors"
	"time"
)

// ErrDecodeBusy refuses work that waited its whole turn for a decoding slot:
// other images are being decoded. Nothing was stored; the same request can
// be retried.
var ErrDecodeBusy = errors.New("media: busy decoding other images")

// DecodeBudget bounds the images core decodes at once, across everything
// that decodes one: uploads for a purpose, SVG rasterizing, cover colours
// and the backfills. A process has one, made at startup and handed to each
// of them, so that together they hold at most Slots decoded images in
// memory (docs/media-lifecycle.md, "Decode budget", has the worst case).
type DecodeBudget struct {
	slots chan struct{}
	svg   chan struct{}
	wait  time.Duration
}

// DecodeBudgetConfig sizes a DecodeBudget. Zero fields take the defaults.
type DecodeBudgetConfig struct {
	// Slots is how many images may be decoded at once; default 2.
	Slots int
	// Wait is how long work waits for a slot before ErrDecodeBusy; default
	// 10 seconds.
	Wait time.Duration
}

// NewDecodeBudget makes a budget. SVG gets one slot of its own on top of
// the shared ones: at most one SVG is sanitized at a time.
func NewDecodeBudget(config DecodeBudgetConfig) *DecodeBudget {
	if config.Slots <= 0 {
		config.Slots = 2
	}
	if config.Wait <= 0 {
		config.Wait = 10 * time.Second
	}
	return &DecodeBudget{
		slots: make(chan struct{}, config.Slots),
		svg:   make(chan struct{}, 1),
		wait:  config.Wait,
	}
}

// Acquire waits for a decoding slot, at most the budget's wait and while
// ctx lasts: ErrDecodeBusy when the wait runs out, ctx's error when ctx
// ends first. release ends the turn.
func (b *DecodeBudget) Acquire(ctx context.Context) (release func(), err error) {
	timer := time.NewTimer(b.wait)
	defer timer.Stop()
	return b.acquire(ctx, timer.C, b.slots)
}

// TryAcquire takes a decoding slot only when one is free now, for work
// that is better skipped than waited for.
func (b *DecodeBudget) TryAcquire() (release func(), ok bool) {
	select {
	case b.slots <- struct{}{}:
		return func() { <-b.slots }, true
	default:
		return nil, false
	}
}

// AcquireSVG waits, within one wait, for the SVG slot (one SVG is
// sanitized at a time) and then a shared slot.
func (b *DecodeBudget) AcquireSVG(ctx context.Context) (release func(), err error) {
	timer := time.NewTimer(b.wait)
	defer timer.Stop()
	releaseSVG, err := b.acquire(ctx, timer.C, b.svg)
	if err != nil {
		return nil, err
	}
	releaseShared, err := b.acquire(ctx, timer.C, b.slots)
	if err != nil {
		releaseSVG()
		return nil, err
	}
	return func() { releaseShared(); releaseSVG() }, nil
}

func (b *DecodeBudget) acquire(ctx context.Context, expired <-chan time.Time, slots chan struct{}) (func(), error) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-expired:
		return nil, ErrDecodeBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
