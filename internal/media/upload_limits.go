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

// DefaultUploadLimits are 100 uploads per 10 minutes and 2 GiB per rolling
// day: an organizer uploading a whole event gallery stays inside them, a
// stolen account is still bounded. The spec's example numbers (30, 500 MB)
// would refuse a gallery upload partway through.
func DefaultUploadLimits() UploadLimits {
	return UploadLimits{Count: 100, CountWindow: 10 * time.Minute, DailyBytes: 2048 << 20}
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

// UploadLimit names a limit of UploadLimits, as the 429 names it.
type UploadLimit string

const (
	// UploadLimitUploads is Count per CountWindow.
	UploadLimitUploads UploadLimit = "uploads"
	// UploadLimitVolume is DailyBytes per rolling day.
	UploadLimitVolume UploadLimit = "volume"
)

const (
	// uploadVolumeWindow is the rolling day DailyBytes is counted over.
	uploadVolumeWindow = 24 * time.Hour
	// uploadSweepInterval is how often Admit lets go of everyone whose
	// uploads have all left their windows.
	uploadSweepInterval = time.Hour
)

// UploadLimitRefusal is an upload refused by its person's budget.
type UploadLimitRefusal struct {
	// Limit is the limit that refused it.
	Limit UploadLimit
	// Budget is the whole budget the person has.
	Budget UploadLimits
	// RetryAfter is how long until the same upload would fit, in whole
	// seconds, at least one.
	RetryAfter time.Duration
}

// UploadLimiter keeps each person's budget in this process's memory: a
// restart forgets it, and each replica of core would keep its own. It is not
// fiber's limiter middleware because it also counts bytes and gives back
// uploads that failed on core's side.
type UploadLimiter struct {
	limits UploadLimits
	now    func() time.Time

	mu sync.Mutex
	// persons holds each person's uploads still inside a window, oldest
	// first. A person with none is not held.
	persons map[uuid.UUID][]chargedUpload
	lastID  uint64
	sweptAt time.Time
}

type chargedUpload struct {
	id    uint64
	at    time.Time
	bytes int64
}

func NewUploadLimiter(limits UploadLimits, now func() time.Time) *UploadLimiter {
	return &UploadLimiter{limits: limits, now: now, persons: map[uuid.UUID][]chargedUpload{}}
}

// UploadCharge is one upload Admit charged to a person's budget.
type UploadCharge struct {
	limiter *UploadLimiter
	person  uuid.UUID
	id      uint64
}

// Admit charges one upload of size bytes to the person's budget. An upload
// that does not fit is not charged: Admit returns the refusal instead, naming
// the limit with the longer wait when both refuse.
func (l *UploadLimiter) Admit(person uuid.UUID, size int64) (UploadCharge, *UploadLimitRefusal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	uploads := since(l.persons[person], now.Add(-l.longestWindow()))

	var refusal *UploadLimitRefusal
	if recent := since(uploads, now.Add(-l.limits.CountWindow)); len(recent) >= l.limits.Count {
		oldest := recent[len(recent)-l.limits.Count]
		refusal = &UploadLimitRefusal{
			Limit: UploadLimitUploads, Budget: l.limits,
			RetryAfter: retryAfter(oldest.at.Add(l.limits.CountWindow).Sub(now)),
		}
	}
	day := since(uploads, now.Add(-uploadVolumeWindow))
	if fitsAt, fits := volumeFitsAt(day, size, l.limits.DailyBytes, now); !fits {
		if wait := retryAfter(fitsAt.Sub(now)); refusal == nil || wait > refusal.RetryAfter {
			refusal = &UploadLimitRefusal{Limit: UploadLimitVolume, Budget: l.limits, RetryAfter: wait}
		}
	}
	if refusal != nil {
		l.hold(person, uploads)
		return UploadCharge{}, refusal
	}
	l.lastID++
	l.hold(person, append(uploads, chargedUpload{id: l.lastID, at: now, bytes: size}))
	return UploadCharge{limiter: l, person: person, id: l.lastID}, nil
}

// Refund gives the upload back to its person's budget, count and bytes, for
// an upload core failed to store. A second refund does nothing.
func (c UploadCharge) Refund() {
	l := c.limiter
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	uploads := l.persons[c.person]
	for i, u := range uploads {
		if u.id == c.id {
			l.hold(c.person, append(uploads[:i:i], uploads[i+1:]...))
			return
		}
	}
}

// sweep drops, at most once per uploadSweepInterval, the uploads that have
// left every window and the people left with none, so memory follows the
// people who uploaded within the longest window.
func (l *UploadLimiter) sweep(now time.Time) {
	if now.Sub(l.sweptAt) < uploadSweepInterval {
		return
	}
	l.sweptAt = now
	cutoff := now.Add(-l.longestWindow())
	for person, uploads := range l.persons {
		l.hold(person, since(uploads, cutoff))
	}
}

// Tracked is how many people the limiter holds uploads for.
func (l *UploadLimiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.persons)
}

// hold keeps a person's uploads, dropping the person once they have none.
func (l *UploadLimiter) hold(person uuid.UUID, uploads []chargedUpload) {
	if len(uploads) == 0 {
		delete(l.persons, person)
		return
	}
	l.persons[person] = uploads
}

func (l *UploadLimiter) longestWindow() time.Duration {
	return max(l.limits.CountWindow, uploadVolumeWindow)
}

// since is the uploads made after cutoff: an upload leaves a window exactly
// one window after it was made.
func since(uploads []chargedUpload, cutoff time.Time) []chargedUpload {
	i := len(uploads)
	for i > 0 && uploads[i-1].at.After(cutoff) {
		i--
	}
	return uploads[i:]
}

// volumeFitsAt reports whether size more bytes fit under budget beside the
// day's uploads, and if not, when enough of them will have left the day.
func volumeFitsAt(day []chargedUpload, size, budget int64, now time.Time) (time.Time, bool) {
	over := size - budget
	for _, u := range day {
		over += u.bytes
	}
	if over <= 0 {
		return now, true
	}
	for _, u := range day {
		over -= u.bytes
		if over <= 0 {
			return u.at.Add(uploadVolumeWindow), false
		}
	}
	// Larger than the whole budget, which only a budget set below the body
	// limit allows: it never fits, so name a full window.
	return now.Add(uploadVolumeWindow), false
}

// retryAfter rounds a wait up to whole seconds, at least one.
func retryAfter(wait time.Duration) time.Duration {
	seconds := (wait + time.Second - 1) / time.Second
	if seconds < 1 {
		seconds = 1
	}
	return seconds * time.Second
}
