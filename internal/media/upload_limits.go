package media

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// UploadLimits is the budget each person has for single-step uploads, shared
// by every route that stores a file sent through core (ADR-0052, media
// redesign ticket 05).
type UploadLimits struct {
	// Count is the most uploads a person may start within CountWindow.
	Count       int
	CountWindow time.Duration
	// DailyBytes is the most bytes a person may send within a rolling day.
	DailyBytes int64
}

// DefaultUploadLimits are the spec's limits: 30 uploads per 10 minutes and
// 500 MiB per day.
func DefaultUploadLimits() UploadLimits {
	return UploadLimits{Count: 30, CountWindow: 10 * time.Minute, DailyBytes: 500 << 20}
}

// UploadLimitsFromEnv reads MEDIA_UPLOAD_RATE_MAX, MEDIA_UPLOAD_RATE_WINDOW
// and MEDIA_UPLOAD_DAILY_MAX_MIB; each one unset keeps its default.
func UploadLimitsFromEnv(getenv func(string) string) (UploadLimits, error) {
	limits := DefaultUploadLimits()
	if raw := strings.TrimSpace(getenv("MEDIA_UPLOAD_RATE_MAX")); raw != "" {
		count, err := strconv.Atoi(raw)
		if err != nil || count <= 0 {
			return UploadLimits{}, fmt.Errorf("MEDIA_UPLOAD_RATE_MAX must be a positive integer")
		}
		limits.Count = count
	}
	if raw := strings.TrimSpace(getenv("MEDIA_UPLOAD_RATE_WINDOW")); raw != "" {
		window, err := time.ParseDuration(raw)
		if err != nil || window <= 0 {
			return UploadLimits{}, fmt.Errorf("MEDIA_UPLOAD_RATE_WINDOW must be a positive duration")
		}
		limits.CountWindow = window
	}
	if raw := strings.TrimSpace(getenv("MEDIA_UPLOAD_DAILY_MAX_MIB")); raw != "" {
		mib, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || mib <= 0 {
			return UploadLimits{}, fmt.Errorf("MEDIA_UPLOAD_DAILY_MAX_MIB must be a positive integer")
		}
		limits.DailyBytes = mib << 20
	}
	return limits, nil
}

// UploadLimit names a limit of UploadLimits.
type UploadLimit string

const (
	UploadLimitCount  UploadLimit = "uploads"
	UploadLimitVolume UploadLimit = "volume"
)

// uploadVolumeWindow is the rolling day DailyBytes is counted over.
const uploadVolumeWindow = 24 * time.Hour

// UploadLimitRefusal is the limit that refused an upload.
type UploadLimitRefusal struct {
	Limit  UploadLimit
	Max    int64
	Window time.Duration
	// RetryAfter is how long until the same upload would fit, in whole
	// seconds, at least one.
	RetryAfter time.Duration
}

// UploadLimiter keeps each person's budget in this process's memory: a
// restart forgets it, and each replica of core would keep its own. A person
// is kept until the process ends, with only their uploads still inside a
// window, so memory follows the number of people who upload.
type UploadLimiter struct {
	limits UploadLimits
	now    func() time.Time

	mu      sync.Mutex
	persons map[uuid.UUID]*uploadHistory
}

type uploadHistory struct {
	// starts are the uploads still inside the count window, oldest first.
	starts []time.Time
	// sent are the uploads still inside the volume window, oldest first,
	// and sentBytes their total.
	sent      []sentUpload
	sentBytes int64
}

type sentUpload struct {
	at    time.Time
	bytes int64
}

func NewUploadLimiter(limits UploadLimits, now func() time.Time) *UploadLimiter {
	return &UploadLimiter{limits: limits, now: now, persons: map[uuid.UUID]*uploadHistory{}}
}

// Admit charges one upload of size bytes to the person's budget. An upload
// that does not fit is not charged: Admit reports false with the limit that
// refused it, the one with the longer wait when both do.
func (l *UploadLimiter) Admit(person uuid.UUID, size int64) (UploadLimitRefusal, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	h := l.persons[person]
	if h == nil {
		h = &uploadHistory{}
		l.persons[person] = h
	}
	h.starts = dropBefore(h.starts, now.Add(-l.limits.CountWindow))
	h.dropSentBefore(now.Add(-uploadVolumeWindow))

	var refusal UploadLimitRefusal
	if len(h.starts) >= l.limits.Count {
		refusal = UploadLimitRefusal{
			Limit: UploadLimitCount, Max: int64(l.limits.Count), Window: l.limits.CountWindow,
			RetryAfter: retryAfter(h.starts[len(h.starts)-l.limits.Count].Add(l.limits.CountWindow).Sub(now)),
		}
	}
	if h.sentBytes+size > l.limits.DailyBytes {
		wait := retryAfter(h.fitsAt(now, size, l.limits.DailyBytes).Sub(now))
		if wait > refusal.RetryAfter {
			refusal = UploadLimitRefusal{
				Limit: UploadLimitVolume, Max: l.limits.DailyBytes, Window: uploadVolumeWindow, RetryAfter: wait,
			}
		}
	}
	if refusal.Limit != "" {
		return refusal, false
	}
	h.starts = append(h.starts, now)
	h.sent = append(h.sent, sentUpload{at: now, bytes: size})
	h.sentBytes += size
	return UploadLimitRefusal{}, true
}

func (h *uploadHistory) dropSentBefore(cutoff time.Time) {
	i := 0
	for i < len(h.sent) && !h.sent[i].at.After(cutoff) {
		h.sentBytes -= h.sent[i].bytes
		i++
	}
	h.sent = h.sent[i:]
}

// fitsAt is when enough of the sent uploads have left the volume window for
// size more bytes to fit under max.
func (h *uploadHistory) fitsAt(now time.Time, size, max int64) time.Time {
	over := h.sentBytes + size - max
	for _, s := range h.sent {
		over -= s.bytes
		if over <= 0 {
			return s.at.Add(uploadVolumeWindow)
		}
	}
	// Larger than the whole budget, which only a budget set below the body
	// limit allows: it never fits, so name a full window.
	return now.Add(uploadVolumeWindow)
}

// dropBefore drops the times at or before cutoff: an upload leaves its window
// exactly one window after it was made.
func dropBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && !times[i].After(cutoff) {
		i++
	}
	return times[i:]
}

// retryAfter rounds a wait up to whole seconds, at least one.
func retryAfter(wait time.Duration) time.Duration {
	seconds := (wait + time.Second - 1) / time.Second
	if seconds < 1 {
		seconds = 1
	}
	return seconds * time.Second
}
