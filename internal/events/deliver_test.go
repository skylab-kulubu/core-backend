package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	amqp "github.com/rabbitmq/amqp091-go"
)

// memoryStore is Store in memory: a record is kept only when fn succeeds.
type memoryStore struct {
	mu      sync.Mutex
	records map[string]bool
}

func (s *memoryStore) Consume(_ context.Context, consumer, source, id string, fn func(pgx.Tx) error) (bool, error) {
	key := consumer + "\x00" + source + "\x00" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = map[string]bool{}
	}
	if s.records[key] {
		return true, nil
	}
	if err := fn(nil); err != nil {
		return false, err
	}
	s.records[key] = true
	return false, nil
}

func (s *memoryStore) DeleteBefore(context.Context, time.Time) (int64, error) { return 0, nil }

// settlement is what a delivery was answered with.
type settlement struct {
	mu      sync.Mutex
	answers []string
}

func (a *settlement) Ack(uint64, bool) error { a.add("ack"); return nil }
func (a *settlement) Nack(_ uint64, _ bool, requeue bool) error {
	a.add(fmt.Sprintf("nack requeue=%v", requeue))
	return nil
}
func (a *settlement) Reject(_ uint64, requeue bool) error {
	a.add(fmt.Sprintf("reject requeue=%v", requeue))
	return nil
}
func (a *settlement) add(s string) { a.mu.Lock(); a.answers = append(a.answers, s); a.mu.Unlock() }
func (a *settlement) got() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.answers, ",")
}

func delivery(body string, redelivered bool) (amqp.Delivery, *settlement) {
	s := &settlement{}
	return amqp.Delivery{Acknowledger: s, Body: []byte(body), ContentType: CloudEventsContentType, Redelivered: redelivered}, s
}

func event(id string) string {
	return `{"specversion":"1.0","id":"` + id + `","source":"/forms","type":"forms.form.updated","data":{"formId":"f"}}`
}

func testConsumer(t *testing.T, store Store, handle Handler) (*Consumer, *subscription, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var logs []string
	c := NewConsumer(Config{Enabled: true, Host: "h", Port: 5672, Vhost: "v", Username: "u", Password: "the-password"}, store, Options{
		RetryDelays: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		Logf: func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		},
	})
	if err := c.Subscribe(Subscription{Queue: "core.test", Handle: handle}); err != nil {
		t.Fatal(err)
	}
	return c, c.subs[0], &logs
}

func TestDeliverOutcomes(t *testing.T) {
	transient := errors.New("database is down")
	for name, tc := range map[string]struct {
		body    string
		handle  func(calls int) error
		answer  string
		calls   int
		metrics []string
	}{
		"processed": {
			body: event("1"), handle: func(int) error { return nil }, answer: "ack", calls: 1,
			metrics: []string{`skylab_events_consumed_total{queue="core.test",outcome="processed"} 1`},
		},
		"unrelated": {
			body: event("1"), handle: func(int) error { return fmt.Errorf("form f: %w", ErrUnrelated) }, answer: "ack", calls: 1,
			metrics: []string{`skylab_events_consumed_total{queue="core.test",outcome="unrelated"} 1`},
		},
		"invalid envelope": {
			body: `{"id":"1"}`, handle: func(int) error { return nil }, answer: "reject requeue=false", calls: 0,
			metrics: []string{
				`skylab_events_consumed_total{queue="core.test",outcome="dead_lettered"} 1`,
				`skylab_events_dlq_total{queue="core.test",reason="invalid_envelope"} 1`,
			},
		},
		"permanent": {
			body: event("1"), handle: func(int) error { return Permanent(errors.New("unknown schema version")) }, answer: "nack requeue=false", calls: 1,
			metrics: []string{`skylab_events_dlq_total{queue="core.test",reason="permanent"} 1`, `skylab_events_retries_total{queue="core.test"} 0`},
		},
		"transient, then fine": {
			body: event("1"),
			handle: func(calls int) error {
				if calls < 3 {
					return transient
				}
				return nil
			},
			answer: "ack", calls: 3,
			metrics: []string{`skylab_events_consumed_total{queue="core.test",outcome="processed"} 1`, `skylab_events_retries_total{queue="core.test"} 2`},
		},
		"retries exhausted": {
			body: event("1"), handle: func(int) error { return transient }, answer: "nack requeue=false", calls: 4,
			metrics: []string{`skylab_events_dlq_total{queue="core.test",reason="retries_exhausted"} 1`, `skylab_events_retries_total{queue="core.test"} 3`},
		},
		"panic": {
			body: event("1"), handle: func(int) error { panic("index out of range") }, answer: "nack requeue=false", calls: 1,
			metrics: []string{`skylab_events_dlq_total{queue="core.test",reason="handler_panic"} 1`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			store := &memoryStore{}
			c, s, _ := testConsumer(t, store, func(_ context.Context, _ pgx.Tx, e Event) error {
				calls++
				if e.Queue != "core.test" || e.Source != "/forms" {
					t.Errorf("event = %+v", e)
				}
				return tc.handle(calls)
			})
			d, answers := delivery(tc.body, false)
			c.deliver(context.Background(), s, d)
			if answers.got() != tc.answer || calls != tc.calls {
				t.Fatalf("answer %q after %d calls, want %q after %d", answers.got(), calls, tc.answer, tc.calls)
			}
			text := c.Prometheus()
			for _, line := range tc.metrics {
				if !strings.Contains(text, line+"\n") {
					t.Fatalf("metrics lack %s:\n%s", line, text)
				}
			}
			// A failed or unrelated event leaves no record: a later
			// delivery of it is handled again.
			if tc.answer != "ack" || name == "unrelated" {
				if len(store.records) != 0 {
					t.Fatalf("records = %v", store.records)
				}
			}
		})
	}
}

// The same source and id a second time is acked as a duplicate and not
// handled again.
func TestDeliverDuplicate(t *testing.T) {
	calls := 0
	c, s, _ := testConsumer(t, &memoryStore{}, func(context.Context, pgx.Tx, Event) error { calls++; return nil })
	first, a1 := delivery(event("same"), false)
	again, a2 := delivery(event("same"), true)
	c.deliver(context.Background(), s, first)
	c.deliver(context.Background(), s, again)
	if calls != 1 || a1.got() != "ack" || a2.got() != "ack" {
		t.Fatalf("calls %d, answers %q %q", calls, a1.got(), a2.got())
	}
	text := c.Prometheus()
	for _, line := range []string{
		`skylab_events_consumed_total{queue="core.test",outcome="processed"} 1`,
		`skylab_events_consumed_total{queue="core.test",outcome="duplicate"} 1`,
		`skylab_events_redelivered_total{queue="core.test"} 1`,
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("metrics lack %s:\n%s", line, text)
		}
	}
}

// A shutdown during the retries leaves the delivery unsettled: the closing
// channel returns it to the queue, and it is not sent to the DLQ.
func TestDeliverStopsWithoutSettlingOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, s, _ := testConsumer(t, &memoryStore{}, func(context.Context, pgx.Tx, Event) error {
		cancel()
		return errors.New("canceled")
	})
	c.retryDelays = []time.Duration{time.Hour}
	d, answers := delivery(event("1"), false)
	done := make(chan struct{})
	go func() { c.deliver(ctx, s, d); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deliver did not stop with its context")
	}
	if answers.got() != "" {
		t.Fatalf("settled with %q", answers.got())
	}
}

// Log lines name the queue, the type and the id, never the password, and
// the event's data does not reach them.
func TestDeliverLogsNoData(t *testing.T) {
	c, s, logs := testConsumer(t, &memoryStore{}, func(context.Context, pgx.Tx, Event) error {
		return Permanent(errors.New("bad"))
	})
	d, _ := delivery(`{"specversion":"1.0","id":"1","source":"/forms","type":"t","data":{"email":"someone@example.com"}}`, false)
	c.deliver(context.Background(), s, d)
	if len(*logs) == 0 {
		t.Fatal("no log line")
	}
	for _, line := range *logs {
		if strings.Contains(line, "someone@example.com") || strings.Contains(line, "the-password") {
			t.Fatalf("log line carries data: %s", line)
		}
	}
}

// A handler's error reaches the log line without the personal data it may
// quote: a PostgreSQL message or detail, an e-mail or IP address in the
// handler's own text, a panic's value. The ids the handler wrapped stay.
func TestDeliverLogsNoPersonalDataFromHandlerErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		fail func() error
		want string
	}{
		"a PostgreSQL message": {
			fail: func() error {
				return Permanent(fmt.Errorf("ticket for response r-1: %w", &pgconn.PgError{
					Severity: "ERROR", Code: "22P02", Message: `invalid input syntax for type uuid: "ayse@example.com"`,
				}))
			},
			want: `ticket for response r-1: PostgreSQL error (SQLSTATE 22P02)`,
		},
		"a PostgreSQL key detail": {
			fail: func() error {
				return Permanent(&pgconn.PgError{
					Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint \"users_email_key\"",
					Detail: "Key (email)=(ayse@example.com) already exists.", ConstraintName: "users_email_key",
				})
			},
			want: `PostgreSQL error (SQLSTATE 23505, constraint users_email_key)`,
		},
		"the handler's own text": {
			fail: func() error { return Permanent(fmt.Errorf("guest ayse@example.com from 203.0.113.7: refused")) },
			want: `guest [e-mail] from [ip]: refused`,
		},
		"a panic": {
			fail: func() error { panic("no ticket for ayse@example.com") },
			want: `handler panicked with a string`,
		},
		"a runtime panic": {
			fail: func() error { var m map[string]int; m["x"]++; return nil },
			want: `handler panicked: assignment to entry in nil map`,
		},
		"a retry": {
			fail: func() error { return errors.New("peer 203.0.113.7 answered 503") },
			want: `peer [ip] answered 503`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, s, logs := testConsumer(t, &memoryStore{}, func(context.Context, pgx.Tx, Event) error { return tc.fail() })
			d, _ := delivery(event("1"), false)
			c.deliver(context.Background(), s, d)
			found := false
			for _, line := range *logs {
				if strings.Contains(line, "ayse") || strings.Contains(line, "203.0.113.7") {
					t.Fatalf("log line carries personal data: %s", line)
				}
				found = found || strings.Contains(line, tc.want)
			}
			if !found {
				t.Fatalf("no log line says %q: %v", tc.want, *logs)
			}
		})
	}
}

func TestSubscribeRules(t *testing.T) {
	c := NewConsumer(Config{}, &memoryStore{}, Options{})
	ok := func(context.Context, pgx.Tx, Event) error { return nil }
	if err := c.Subscribe(Subscription{Queue: "core.forms"}); err == nil {
		t.Fatal("accepted a subscription without a handler")
	}
	if err := c.Subscribe(Subscription{Handle: ok}); err == nil {
		t.Fatal("accepted a subscription without a queue")
	}
	if err := c.Subscribe(Subscription{Queue: "core.forms", Handle: ok}); err != nil {
		t.Fatal(err)
	}
	if c.subs[0].Consumer != "core.forms" || c.subs[0].Decode == nil {
		t.Fatalf("defaults = %+v", c.subs[0])
	}
	if err := c.Subscribe(Subscription{Queue: "core.forms", Handle: ok}); err == nil {
		t.Fatal("accepted a queue twice")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	<-c.Run(ctx)
	if err := c.Subscribe(Subscription{Queue: "core.keycloak", Handle: ok}); err == nil {
		t.Fatal("accepted a subscription after Run")
	}
}

// Every log line about an event carries its trace id when the publisher sent
// a valid traceparent (handled, unrelated, retried, dead-lettered), and none
// when it did not. The trace id is the only part of it a line carries.
func TestDeliverLogsTraceID(t *testing.T) {
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	const traceID = `"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"`
	traced := func(id string) string {
		return `{"specversion":"1.0","id":"` + id + `","source":"/forms","type":"forms.form.updated","traceparent":"` + traceparent + `","data":{"formId":"f"}}`
	}
	transient := errors.New("database is down")
	for name, tc := range map[string]struct {
		handle func(calls int) error
		events []string
	}{
		"processed": {handle: func(int) error { return nil }, events: []string{"events_handled"}},
		"unrelated": {handle: func(int) error { return ErrUnrelated }, events: []string{"events_handled"}},
		"retried, then fine": {
			handle: func(calls int) error {
				if calls < 2 {
					return transient
				}
				return nil
			},
			events: []string{"events_retry", "events_handled"},
		},
		"dead-lettered": {handle: func(int) error { return Permanent(errors.New("bad")) }, events: []string{"events_dead_lettered"}},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			c, s, logs := testConsumer(t, &memoryStore{}, func(context.Context, pgx.Tx, Event) error { calls++; return tc.handle(calls) })
			d, _ := delivery(traced("1"), false)
			c.deliver(context.Background(), s, d)
			if len(*logs) != len(tc.events) {
				t.Fatalf("log lines %v, want %v", *logs, tc.events)
			}
			for i, line := range *logs {
				if !strings.Contains(line, `"event":"`+tc.events[i]+`"`) || !strings.Contains(line, traceID) {
					t.Fatalf("line %d = %s, want %s with %s", i, line, tc.events[i], traceID)
				}
				if strings.Contains(line, "00f067aa0ba902b7") {
					t.Fatalf("line carries the parent id: %s", line)
				}
			}

			// The same event without traceparent: the same lines, no trace id.
			calls = 0
			c, s, logs = testConsumer(t, &memoryStore{}, func(context.Context, pgx.Tx, Event) error { calls++; return tc.handle(calls) })
			d, _ = delivery(event("1"), false)
			c.deliver(context.Background(), s, d)
			if len(*logs) != len(tc.events) {
				t.Fatalf("log lines %v, want %v", *logs, tc.events)
			}
			for _, line := range *logs {
				if strings.Contains(line, "trace_id") {
					t.Fatalf("line without a traceparent has a trace id: %s", line)
				}
			}
		})
	}
}

// A duplicate is logged as handled with its outcome.
func TestDeliverLogsHandledOutcome(t *testing.T) {
	c, s, logs := testConsumer(t, &memoryStore{}, func(context.Context, pgx.Tx, Event) error { return nil })
	first, _ := delivery(event("same"), false)
	again, _ := delivery(event("same"), true)
	c.deliver(context.Background(), s, first)
	c.deliver(context.Background(), s, again)
	want := []string{
		`{"event":"events_handled","queue":"core.test","type":"forms.form.updated","id":"same","outcome":"processed"}`,
		`{"event":"events_handled","queue":"core.test","type":"forms.form.updated","id":"same","outcome":"duplicate"}`,
	}
	if strings.Join(*logs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("log lines:\n%s\nwant:\n%s", strings.Join(*logs, "\n"), strings.Join(want, "\n"))
	}
}
