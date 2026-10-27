package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// GroupsChangedChannel is the Postgres channel core's processes tell each
// other on that a person's Groups, or everyone's, changed through core.
const GroupsChangedChannel = "identity_groups_changed"

const (
	// groupsNotifyTimeout bounds telling the other processes of one write.
	groupsNotifyTimeout = 2 * time.Second
	// defaultGroupsListenRetry is the pause before listening again after the
	// listening connection broke.
	defaultGroupsListenRetry = 5 * time.Second
)

// Notifier sends one statement: the shared pool, in core.
type Notifier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// SharedGroupCacheOptions tune SharedGroupCache. Zero values take the
// defaults.
type SharedGroupCacheOptions struct {
	Logf func(format string, args ...any)
	// Origin names this process in what it says; it ignores its own words.
	// Empty: a random one.
	Origin string
	// Retry is the pause before listening again. Zero: 5 s.
	Retry time.Duration
	// Listening is told each time the process listens again (tests wait on
	// it). Nil: nobody.
	Listening func()
}

// SharedGroupCache spreads the forgets of a process's GroupCache to every
// core process (horizontal-scale ticket 26). Forget and ForgetAll drop the
// local answers at once and NOTIFY GroupsChangedChannel; Run LISTENs on a
// connection of its own and drops what another process said changed. While
// the listening connection is down nothing arrives, so each time it listens
// again it first forgets everyone: a write it missed is not answered from
// before it. Payload: "<origin> <user id>" or "<origin> *". The channel
// carries a Keycloak id, never a name or a path.
type SharedGroupCache struct {
	local     GroupCache
	notify    Notifier
	connect   func(context.Context) (*pgx.Conn, error)
	origin    string
	logf      func(string, ...any)
	retry     time.Duration
	listening func()
}

func NewSharedGroupCache(local GroupCache, notify Notifier, connect func(context.Context) (*pgx.Conn, error), options SharedGroupCacheOptions) *SharedGroupCache {
	c := &SharedGroupCache{
		local: local, notify: notify, connect: connect,
		origin: options.Origin, logf: options.Logf, retry: options.Retry, listening: options.Listening,
	}
	if c.origin == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		c.origin = hex.EncodeToString(b[:])
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	if c.retry <= 0 {
		c.retry = defaultGroupsListenRetry
	}
	return c
}

func (c *SharedGroupCache) Forget(userID uuid.UUID) {
	c.local.Forget(userID)
	c.tell(userID.String())
}

func (c *SharedGroupCache) ForgetAll() {
	c.local.ForgetAll()
	c.tell("*")
}

// tell is best effort: a process that does not hear it answers from its
// cache until its minute is up, as before.
func (c *SharedGroupCache) tell(target string) {
	ctx, cancel := context.WithTimeout(context.Background(), groupsNotifyTimeout)
	defer cancel()
	if _, err := c.notify.Exec(ctx, `SELECT pg_notify($1, $2)`, GroupsChangedChannel, c.origin+" "+target); err != nil {
		c.logf("ERROR group cache: could not tell the other core processes of a Group change: %v", err)
	}
}

// Run listens until ctx ends; the channel closes when it has stopped.
func (c *SharedGroupCache) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			err := c.listen(ctx)
			if ctx.Err() != nil {
				return
			}
			c.logf("ERROR group cache: not listening for other core processes' Group changes, again in %s: %v", c.retry, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.retry):
			}
		}
	}()
	return done
}

func (c *SharedGroupCache) listen(ctx context.Context) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(closing)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+GroupsChangedChannel); err != nil {
		return err
	}
	c.local.ForgetAll()
	if c.listening != nil {
		c.listening()
	}
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		c.apply(notification.Payload)
	}
}

func (c *SharedGroupCache) apply(payload string) {
	origin, target, ok := strings.Cut(payload, " ")
	if !ok || origin == c.origin {
		return
	}
	if target == "*" {
		c.local.ForgetAll()
		return
	}
	if id, err := uuid.Parse(target); err == nil {
		c.local.Forget(id)
	}
}
