package media

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DirectUploadLimits is each person's budget for the Direct uploads core
// starts, apart from the single-step one (decision Q23: the product that
// grants a Direct upload limits it; core is that product for its own).
type DirectUploadLimits struct {
	// MaxOpen is the most Direct uploads a person may have open at once:
	// started, neither completed, refused nor expired.
	MaxOpen int
	// DailyBytes is the most bytes a person may declare in Direct uploads
	// within a rolling day.
	DailyBytes int64
}

// DefaultDirectUploadLimits are 3 uploads open at once and 10 GiB a day: a
// club's large files and recordings of an event day fit, a stolen account is
// bounded.
func DefaultDirectUploadLimits() DirectUploadLimits {
	return DirectUploadLimits{MaxOpen: 3, DailyBytes: 10 << 30}
}

// DirectUploadLimitsFromEnv reads MEDIA_DIRECT_UPLOAD_MAX_OPEN and
// MEDIA_DIRECT_UPLOAD_DAILY_MAX_MIB; each one unset keeps its default.
func DirectUploadLimitsFromEnv(getenv func(string) string) (DirectUploadLimits, error) {
	limits := DefaultDirectUploadLimits()
	if raw := strings.TrimSpace(getenv("MEDIA_DIRECT_UPLOAD_MAX_OPEN")); raw != "" {
		open, err := strconv.Atoi(raw)
		if err != nil || open <= 0 {
			return DirectUploadLimits{}, fmt.Errorf("MEDIA_DIRECT_UPLOAD_MAX_OPEN must be a positive integer")
		}
		limits.MaxOpen = open
	}
	if raw := strings.TrimSpace(getenv("MEDIA_DIRECT_UPLOAD_DAILY_MAX_MIB")); raw != "" {
		mib, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || mib <= 0 {
			return DirectUploadLimits{}, fmt.Errorf("MEDIA_DIRECT_UPLOAD_DAILY_MAX_MIB must be a positive integer")
		}
		limits.DailyBytes = mib << 20
	}
	return limits, nil
}

// UploadLimitOpen is DirectUploadLimits.MaxOpen, as the 429 names it.
const UploadLimitOpen UploadLimit = "open"

// DirectUploadLimitRefusal is a Direct upload its person's budget refuses:
// nothing is started and nothing is charged.
type DirectUploadLimitRefusal struct {
	// Limit is UploadLimitOpen or UploadLimitVolume.
	Limit  UploadLimit
	Limits DirectUploadLimits
	// RetryAfter is how long until the same upload would fit, in whole
	// seconds, at least one: until the first open upload expires, or the
	// day has room.
	RetryAfter time.Duration
}

func (r *DirectUploadLimitRefusal) Error() string {
	return fmt.Sprintf("media: the person's Direct upload limit (%s) is reached; retry after %s", r.Limit, r.RetryAfter)
}

// TooManyOpenDirectUploads is StageDirectUpload's refusal: the person has
// the most uploads open already. NextExpiry is when the first of them
// expires.
type TooManyOpenDirectUploads struct {
	NextExpiry time.Time
}

func (e *TooManyOpenDirectUploads) Error() string {
	return "media: too many Direct uploads open"
}

// DirectUploadLimiter keeps each person's daily Direct upload volume in this
// process's memory, as UploadLimiter keeps the single-step budget (a restart
// forgets it; each replica keeps its own). The open uploads are counted in
// the store, where they are (StageDirectUpload), so that limit holds across
// restarts.
//
// A start is charged its declared size. The charge stays, as a single-step
// refusal's does, when the file is refused at completion or the upload is
// never completed: the bytes were sent. It is given back only when core
// fails (a 5xx), at the start or after the parts were joined.
type DirectUploadLimiter struct {
	limits DirectUploadLimits
	volume *UploadLimiter

	mu sync.Mutex
	// charges holds each started upload's charge until the upload ends.
	charges map[uuid.UUID]heldCharge
}

type heldCharge struct {
	charge UploadCharge
	at     time.Time
}

func NewDirectUploadLimiter(limits DirectUploadLimits, now func() time.Time) *DirectUploadLimiter {
	return &DirectUploadLimiter{
		limits: limits,
		// Only the volume: the count is MaxOpen's, kept in the store.
		volume:  NewUploadLimiter(UploadLimits{Count: math.MaxInt, CountWindow: uploadVolumeWindow, DailyBytes: limits.DailyBytes}, now),
		charges: map[uuid.UUID]heldCharge{},
	}
}

// maxOpen is MaxOpen; 0 (no limit) without a limiter.
func (l *DirectUploadLimiter) maxOpen() int {
	if l == nil {
		return 0
	}
	return l.limits.MaxOpen
}

// charge charges upload id's declared size to the person's day, or refuses
// it without charging.
func (l *DirectUploadLimiter) charge(person, id uuid.UUID, size int64, now time.Time) *DirectUploadLimitRefusal {
	if l == nil {
		return nil
	}
	charge, refusal := l.volume.Admit(person, size)
	if refusal != nil {
		return &DirectUploadLimitRefusal{Limit: UploadLimitVolume, Limits: l.limits, RetryAfter: refusal.RetryAfter}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// A charge older than a day counts no more: nothing is left to give
	// back.
	for other, held := range l.charges {
		if now.Sub(held.at) >= uploadVolumeWindow {
			delete(l.charges, other)
		}
	}
	l.charges[id] = heldCharge{charge: charge, at: now}
	return nil
}

// openRefusal refuses an upload over MaxOpen.
func (l *DirectUploadLimiter) openRefusal(tooMany *TooManyOpenDirectUploads, now time.Time) *DirectUploadLimitRefusal {
	return &DirectUploadLimitRefusal{Limit: UploadLimitOpen, Limits: l.limits, RetryAfter: retryAfter(tooMany.NextExpiry.Sub(now))}
}

// refund gives upload id's charge back: core failed it.
func (l *DirectUploadLimiter) refund(id uuid.UUID) {
	if charge, ok := l.take(id); ok {
		charge.Refund()
	}
}

// settle lets upload id's charge stand: the upload ended, completed or
// refused.
func (l *DirectUploadLimiter) settle(id uuid.UUID) {
	l.take(id)
}

func (l *DirectUploadLimiter) take(id uuid.UUID) (UploadCharge, bool) {
	if l == nil {
		return UploadCharge{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	held, ok := l.charges[id]
	delete(l.charges, id)
	return held.charge, ok
}
