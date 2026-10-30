package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GroupsChangedChannel is the Postgres channel core's processes tell each
// other on that a person's Groups, or everyone's, changed through core.
const GroupsChangedChannel = "identity_groups_changed"

const (
	// groupsNotifyTimeout bounds telling the other processes of one write.
	groupsNotifyTimeout = 2 * time.Second
	// defaultGroupsListenRetry is the first pause before listening again
	// after the listening connection broke; it doubles while listening keeps
	// failing, up to maxGroupsListenRetry.
	defaultGroupsListenRetry = 5 * time.Second
	maxGroupsListenRetry     = time.Minute
	// groupsListenIdle is how long the listener waits for a notification
	// before it pings, so a dead connection is noticed within a minute.
	groupsListenIdle = time.Minute
	// groupsPingTimeout bounds that ping: on a half-open connection nothing
	// acknowledges it, and the kernel would retransmit for many minutes.
	groupsPingTimeout = 5 * time.Second
)

// ListenConnect opens the listening connection with the pool's own
// connection settings. Parsing DATABASE_URL again with pgx.ParseConfig would
// send the pool's settings (pool_max_conns…) to Postgres as runtime
// parameters, which it refuses.
func ListenConnect(pool *pgxpool.Pool) func(context.Context) (*pgx.Conn, error) {
	return func(ctx context.Context) (*pgx.Conn, error) {
		return pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	}
}

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
	// Retry is the first pause before listening again; it doubles while
	// listening keeps failing, up to a minute (or Retry, if longer). Zero: 5 s.
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
		// The class only, as in Run: the pool's connection errors name the
		// user, the database and the address.
		c.logf("ERROR group cache: could not tell the other core processes of a Group change (%s)", listenErrorClass(err))
	}
}

// Run listens until ctx ends; the channel closes when it has stopped.
func (c *SharedGroupCache) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		pause := c.retry
		for {
			listened, err := c.listen(ctx)
			if ctx.Err() != nil {
				return
			}
			if listened {
				pause = c.retry
			}
			// The class only: pgx's connection errors name the user, the
			// database and the address.
			c.logf("ERROR group cache: not listening for other core processes' Group changes (%s), again in %s", listenErrorClass(err), pause)
			select {
			case <-ctx.Done():
				return
			case <-time.After(pause):
			}
			pause = min(2*pause, max(maxGroupsListenRetry, c.retry))
		}
	}()
	return done
}

// listen reports whether it got as far as listening, and why it stopped.
func (c *SharedGroupCache) listen(ctx context.Context) (bool, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(closing)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+GroupsChangedChannel); err != nil {
		return false, err
	}
	c.local.ForgetAll()
	if c.listening != nil {
		c.listening()
	}
	for {
		wait, cancel := context.WithTimeout(ctx, groupsListenIdle)
		notification, err := conn.WaitForNotification(wait)
		cancel()
		if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			// Quiet for a minute: make sure the connection is still there.
			ping, cancel := context.WithTimeout(ctx, groupsPingTimeout)
			err := conn.Ping(ping)
			cancel()
			if err != nil {
				return true, err
			}
			continue
		}
		if err != nil {
			return true, err
		}
		c.apply(notification.Payload)
	}
}

func listenErrorClass(err error) string {
	var pgErr *pgconn.PgError
	var connectErr *pgconn.ConnectError
	switch {
	case err == nil:
		return "closed"
	case errors.As(err, &pgErr):
		return "postgres error " + pgErr.Code
	case errors.As(err, &connectErr):
		return "could not connect"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	default:
		return "connection lost"
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
