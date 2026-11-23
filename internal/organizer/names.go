package organizer

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Teams answers the Turkish names of the Teams by code (a Group's name and
// its display_name_tr), as Keycloak holds them; a code without one may be
// left out.
type Teams func(ctx context.Context) (map[string]string, error)

const (
	// DefaultNamesTTL is how long a read of the Teams' names is used
	// before it is read again (in the background, the old one answering
	// meanwhile).
	DefaultNamesTTL = 15 * time.Minute
	// namesRetry is how long a failed read waits before the next one:
	// meanwhile the names already read answer, or the codes.
	namesRetry = time.Minute
	// namesTimeout bounds one read.
	namesTimeout = 5 * time.Second
)

// Names caches the Teams' Turkish names, so a public answer does not read
// Keycloak's Group tree each time (Yusuf, 2026-11-23). The first use reads
// them; a read older than its TTL is read again in the background; a read
// that fails leaves the names it had (none at first: the codes answer).
type Names struct {
	teams   Teams
	ttl     time.Duration
	now     func() time.Time
	onError func(error)

	mu         sync.Mutex
	names      map[string]string
	until      time.Time
	refreshing bool
}

// NewNames caches what teams answers for ttl (DefaultNamesTTL when not
// positive). onError, when set, hears a failed read.
func NewNames(teams Teams, ttl time.Duration, onError func(error)) *Names {
	if ttl <= 0 {
		ttl = DefaultNamesTTL
	}
	return &Names{teams: teams, ttl: ttl, now: time.Now, onError: onError}
}

// Turkish is the Turkish name of the Team code, or "" when it has none or
// the names cannot be read.
func (n *Names) Turkish(code string) string {
	return n.current()[code]
}

func (n *Names) current() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	switch {
	case n.names == nil:
		// The first use: read now, every other caller waits for it.
		n.names, n.until = n.read(now, map[string]string{})
	case !now.Before(n.until) && !n.refreshing:
		n.refreshing = true
		old := n.names
		go func() {
			names, until := n.read(n.now(), old)
			n.mu.Lock()
			n.names, n.until, n.refreshing = names, until, false
			n.mu.Unlock()
		}()
	}
	return n.names
}

// read asks teams once: its answer until the TTL, or, when it fails, old
// until the retry.
func (n *Names) read(now time.Time, old map[string]string) (map[string]string, time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), namesTimeout)
	defer cancel()
	names, err := n.teams(ctx)
	if err != nil {
		if n.onError != nil {
			n.onError(err)
		}
		return old, now.Add(namesRetry)
	}
	clean := make(map[string]string, len(names))
	for code, turkish := range names {
		if turkish = strings.TrimSpace(turkish); turkish != "" {
			clean[strings.TrimSpace(code)] = turkish
		}
	}
	return clean, now.Add(n.ttl)
}

var names atomic.Pointer[Names]

// UseNames makes Name read the Teams' Turkish names from n (core's main
// does it with Keycloak's Groups); nil goes back to the codes alone.
func UseNames(n *Names) {
	names.Store(n)
}
