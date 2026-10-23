package events

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	// DefaultPrefetch is how many unacked deliveries the broker hands one
	// queue's channel (ADR-0071: 20; low volume, a database write each).
	DefaultPrefetch = 20
	// DefaultConcurrency is how many of them are handled at once. Each
	// holds a database connection for its transaction, and core's pool is
	// small: the rest wait in the prefetch.
	DefaultConcurrency = 2
	// minBackoff and maxBackoff bound the wait between two connection (or
	// channel) attempts, which doubles from the first to the second.
	minBackoff = time.Second
	maxBackoff = time.Minute
	// dialTimeout bounds the TCP connect and the AMQP handshake.
	dialTimeout = 10 * time.Second
	// heartbeat is AMQP's keep-alive: a broker gone silent is noticed in
	// about two of them.
	heartbeat = 10 * time.Second
)

// DefaultRetryDelays are the waits before the in-process retries of a
// transient failure (ADR-0071 §3.2: 1 s, 5 s, 30 s); after the last the
// delivery goes to the DLQ.
var DefaultRetryDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

// Subscription is one queue core reads and what it does with each event.
type Subscription struct {
	// Queue is the queue's name (core.forms); ops declares it.
	Queue string
	// Consumer names the events' records; empty is Queue.
	Consumer string
	// Decode reads a delivery; nil is DecodeCloudEvent.
	Decode Decoder
	// Handle does the work.
	Handle Handler
}

// Options are a Consumer's settings; zero values take the defaults.
type Options struct {
	Prefetch    int
	Concurrency int
	// RetryDelays replaces DefaultRetryDelays; tests shorten them.
	RetryDelays []time.Duration
	// MinBackoff and MaxBackoff replace the reconnection bounds.
	MinBackoff, MaxBackoff time.Duration
	// Logf writes the log lines; nil logs nothing.
	Logf func(format string, args ...any)
}

// Consumer keeps a connection to the broker and a channel per subscription,
// and handles their deliveries. Subscribe before Run.
type Consumer struct {
	config      Config
	store       Store
	prefetch    int
	concurrency int
	retryDelays []time.Duration
	minBackoff  time.Duration
	maxBackoff  time.Duration
	logf        func(string, ...any)
	// dial opens the connection; tests point it elsewhere.
	dial func(Config) (*amqp.Connection, error)

	mu      sync.Mutex
	started bool
	subs    []*subscription
	metrics *metrics
}

// subscription is a Subscription with its own counters' key.
type subscription struct {
	Subscription
}

// NewConsumer builds a consumer that records events in store.
func NewConsumer(config Config, store Store, options Options) *Consumer {
	c := &Consumer{
		config: config, store: store,
		prefetch: options.Prefetch, concurrency: options.Concurrency,
		retryDelays: options.RetryDelays,
		minBackoff:  options.MinBackoff, maxBackoff: options.MaxBackoff,
		logf:    options.Logf,
		dial:    dial,
		metrics: newMetrics(),
	}
	if c.prefetch <= 0 {
		c.prefetch = DefaultPrefetch
	}
	if c.concurrency <= 0 {
		c.concurrency = DefaultConcurrency
	}
	if c.retryDelays == nil {
		c.retryDelays = DefaultRetryDelays
	}
	if c.minBackoff <= 0 {
		c.minBackoff = minBackoff
	}
	if c.maxBackoff < c.minBackoff {
		c.maxBackoff = max(maxBackoff, c.minBackoff)
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	return c
}

// Subscribe adds a queue. It fails after Run, for a queue already added, and
// without a queue name or a handler.
func (c *Consumer) Subscribe(s Subscription) error {
	if strings.TrimSpace(s.Queue) == "" || s.Handle == nil {
		return errors.New("events: a subscription needs a queue and a handler")
	}
	if s.Consumer == "" {
		s.Consumer = s.Queue
	}
	if len(s.Consumer) > 100 {
		return errors.New("events: a consumer name is at most 100 bytes")
	}
	if s.Decode == nil {
		s.Decode = DecodeCloudEvent
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("events: subscribe before Run")
	}
	for _, have := range c.subs {
		if have.Queue == s.Queue {
			return fmt.Errorf("events: queue %q subscribed twice", s.Queue)
		}
	}
	c.subs = append(c.subs, &subscription{s})
	c.metrics.addQueue(s.Queue)
	return nil
}

// dial connects with the user's credentials passed as SASL PLAIN, never in a
// URL, so no error or log line can carry the password.
func dial(config Config) (*amqp.Connection, error) {
	properties := amqp.NewConnectionProperties()
	properties.SetClientConnectionName("core-backend")
	return amqp.DialConfig("amqp://"+config.addr()+"/", amqp.Config{
		SASL:       []amqp.Authentication{&amqp.PlainAuth{Username: config.Username, Password: config.Password}},
		Vhost:      config.Vhost,
		Heartbeat:  heartbeat,
		Properties: properties,
		Dial:       amqp.DefaultDial(dialTimeout),
	})
}

// Run connects, consumes every subscription, and reconnects with a growing
// wait whenever the connection is lost or cannot be made, until ctx ends. It
// never blocks the caller: a broker that is down leaves core running, the
// log and /v1/metrics saying so. The channel closes once the connection is
// closed and every handler has returned; a delivery not acked by then goes
// back to its queue.
func (c *Consumer) Run(ctx context.Context) <-chan struct{} {
	c.mu.Lock()
	c.started = true
	subs := append([]*subscription(nil), c.subs...)
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait := backoff{min: c.minBackoff, max: c.maxBackoff}
		for ctx.Err() == nil {
			conn, err := c.connect(ctx)
			if ctx.Err() != nil {
				if conn != nil {
					_ = conn.Close()
				}
				return
			}
			if err != nil {
				c.metrics.connectFailed()
				c.logf(`{"event":"events_connect_failed","level":"warn","broker":%q,"error":%q}`, c.config.addr(), c.redact(err))
				sleep(ctx, wait.next())
				continue
			}
			wait.reset()
			c.metrics.setConnected(true)
			c.logf(`{"event":"events_connected","broker":%q,"vhost":%q,"queues":%d}`, c.config.addr(), c.config.Vhost, len(subs))
			lost := c.serve(ctx, conn, subs)
			c.metrics.setConnected(false)
			_ = conn.Close()
			if ctx.Err() != nil {
				return
			}
			c.logf(`{"event":"events_connection_lost","level":"warn","error":%q}`, c.redact(lost))
			sleep(ctx, wait.next())
		}
	}()
	return done
}

// connect dials without holding up a shutdown: when ctx ends first, the
// connection that may still come is closed.
func (c *Consumer) connect(ctx context.Context) (*amqp.Connection, error) {
	type result struct {
		conn *amqp.Connection
		err  error
	}
	got := make(chan result, 1)
	go func() {
		conn, err := c.dial(c.config)
		got <- result{conn, err}
	}()
	select {
	case r := <-got:
		return r.conn, r.err
	case <-ctx.Done():
		go func() {
			if r := <-got; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// serve consumes every subscription on conn until the connection closes or
// ctx ends, and returns once every subscription has stopped.
func (c *Consumer) serve(ctx context.Context, conn *amqp.Connection, subs []*subscription) error {
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	connCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Go(func() { c.consume(connCtx, conn, s) })
	}
	var lost error
	select {
	case <-ctx.Done():
	case amqpErr, ok := <-closed:
		lost = errors.New("connection closed")
		if ok && amqpErr != nil {
			lost = amqpErr
		}
	}
	// A lost connection takes its channels with it: the handlers still
	// running are stopped (their transactions roll back) and their
	// deliveries return to the queue, to be delivered again.
	cancel()
	wg.Wait()
	return lost
}

// consume reads one subscription's queue on its own channel, opening the
// channel again with a growing wait when the broker closes it (a queue that
// is missing, a consumer the broker cancelled), until ctx ends.
func (c *Consumer) consume(ctx context.Context, conn *amqp.Connection, s *subscription) {
	wait := backoff{min: c.minBackoff, max: c.maxBackoff}
	for ctx.Err() == nil && !conn.IsClosed() {
		started, err := c.consumeChannel(ctx, conn, s)
		c.metrics.setConsuming(s.Queue, false)
		if ctx.Err() != nil || conn.IsClosed() {
			return
		}
		if started {
			wait.reset()
		}
		c.logf(`{"event":"events_channel_closed","level":"warn","queue":%q,"error":%q}`, s.Queue, c.redact(err))
		sleep(ctx, wait.next())
	}
}

// consumeChannel opens a channel, consumes the queue and handles its
// deliveries, at most concurrency at once, until the channel closes or ctx
// ends. started says the consumer was registered.
func (c *Consumer) consumeChannel(ctx context.Context, conn *amqp.Connection, s *subscription) (started bool, err error) {
	ch, err := conn.Channel()
	if err != nil {
		return false, err
	}
	defer func() { _ = ch.Close() }()
	closed := ch.NotifyClose(make(chan *amqp.Error, 1))
	cancelled := ch.NotifyCancel(make(chan string, 1))
	if err := ch.Qos(c.prefetch, 0, false); err != nil {
		return false, err
	}
	// No declare: the queue is ops' (ADR-0071), and core's user may not
	// configure. A queue that is missing closes the channel (404).
	deliveries, err := ch.Consume(s.Queue, "", false, false, false, false, nil)
	if err != nil {
		return false, err
	}
	c.metrics.setConsuming(s.Queue, true)
	c.logf(`{"event":"events_consuming","queue":%q,"prefetch":%d}`, s.Queue, c.prefetch)

	// The handlers stop with the channel: a delivery of a closed channel
	// can no longer be acked, and the broker delivers it again.
	chCtx, stopHandlers := context.WithCancel(ctx)
	slots := make(chan struct{}, c.concurrency)
	var handlers sync.WaitGroup
	defer handlers.Wait()
	defer stopHandlers()
	for {
		select {
		case <-ctx.Done():
			return true, nil
		case amqpErr, ok := <-closed:
			return true, channelClosed(amqpErr, ok)
		case <-cancelled:
			return true, errConsumerCancelled
		case d, ok := <-deliveries:
			if !ok {
				return true, errors.New("deliveries ended")
			}
			// Both slots may be busy for a while (a handler retrying
			// for 36 seconds): a channel that closes in between stops
			// them now, and d, unsettled, goes back with the rest.
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return true, nil
			case amqpErr, ok := <-closed:
				return true, channelClosed(amqpErr, ok)
			case <-cancelled:
				return true, errConsumerCancelled
			}
			handlers.Go(func() {
				defer func() { <-slots }()
				c.deliver(chCtx, s, d)
			})
		}
	}
}

var errConsumerCancelled = errors.New("the broker cancelled the consumer")

// channelClosed is the error of a channel's close notification.
func channelClosed(amqpErr *amqp.Error, ok bool) error {
	if ok && amqpErr != nil {
		return amqpErr
	}
	return errors.New("channel closed")
}

// redact keeps the password out of an error's text, whatever produced it.
func (c *Consumer) redact(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if c.config.Password != "" {
		text = strings.ReplaceAll(text, c.config.Password, "[redacted]")
	}
	return text
}

// backoff doubles from min to max, with up to a fifth more at random so
// replicas do not reconnect in step.
type backoff struct {
	min, max, current time.Duration
}

func (b *backoff) next() time.Duration {
	if b.current < b.min {
		b.current = b.min
	} else {
		b.current = min(b.current*2, b.max)
	}
	return b.current + time.Duration(rand.Int64N(int64(b.current)/5+1))
}

func (b *backoff) reset() { b.current = 0 }

// sleep waits d or until ctx ends, and reports whether it waited all of d.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
