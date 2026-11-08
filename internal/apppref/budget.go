package apppref

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// writeBudget is each person's WritesPerMinute, in a fixed minute window,
// kept by each replica. Only writes the caller may make are counted: a
// refused client does not spend the person's budget.
type writeBudget struct {
	mu      sync.Mutex
	windows map[uuid.UUID]budgetWindow
}

type budgetWindow struct {
	start time.Time
	count int
}

// budgetSweepAt is the window count past which expired windows are dropped.
const budgetSweepAt = 4096

func newWriteBudget() *writeBudget {
	return &writeBudget{windows: map[uuid.UUID]budgetWindow{}}
}

// take spends one write of person at now, or says how long until the
// window starts again.
func (b *writeBudget) take(person uuid.UUID, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.windows) >= budgetSweepAt {
		for p, w := range b.windows {
			if now.Sub(w.start) >= time.Minute {
				delete(b.windows, p)
			}
		}
	}
	w := b.windows[person]
	if now.Sub(w.start) >= time.Minute {
		w = budgetWindow{start: now}
	}
	if w.count >= WritesPerMinute {
		return RateLimitedError{RetryAfter: w.start.Add(time.Minute).Sub(now)}
	}
	w.count++
	b.windows[person] = w
	return nil
}
