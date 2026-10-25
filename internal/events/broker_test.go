package events_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/testrabbitmq"
)

// The core user of the broker tests, with ADR-0071's permissions: no
// configure, no write, read of core's own queues only.
const (
	coreUser     = "core_test"
	corePassword = "core-test-password"
)

// TestBroker runs the consumer against a real RabbitMQ 4 with the topology
// ops sets up: a topic exchange per publisher, a quorum queue per consumer
// with a delivery limit and at-least-once dead-lettering, its DLQ. One broker
// and one database serve every subtest; each has queues of its own.
func TestBroker(t *testing.T) {
	broker := testrabbitmq.Start(t)
	broker.AddUser(t, coreUser, corePassword, "^$", "^$", `^core\.`)
	pool := migrated(t)
	admin := broker.Admin(t)
	if err := admin.ExchangeDeclare("test.events", "topic", true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := admin.ExchangeDeclare("dlx", "direct", true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}

	config := events.Config{Enabled: true, Host: broker.Host, Port: broker.Port, Vhost: testrabbitmq.Vhost, Username: coreUser, Password: corePassword}

	t.Run("processed once, duplicates dropped", func(t *testing.T) {
		queue := declare(t, broker, "core.once")
		var calls atomic.Int64
		c := start(t, config, pool, queue, func(ctx context.Context, tx pgx.Tx, e events.Event) error {
			calls.Add(1)
			_, err := tx.Exec(ctx, `INSERT INTO event_effects (consumer, event_id) VALUES ($1, $2)`, e.Queue, e.ID)
			return err
		})
		waitFor(t, c, fmt.Sprintf(`skylab_events_consuming{queue=%q} 1`, queue))
		// The same event three times (a publisher's retry), then another.
		for range 3 {
			publish(t, broker, queue, "evt-1", cloudEvent("evt-1"))
		}
		publish(t, broker, queue, "evt-2", cloudEvent("evt-2"))
		waitFor(t, c,
			fmt.Sprintf(`skylab_events_consumed_total{queue=%q,outcome="processed"} 2`, queue),
			fmt.Sprintf(`skylab_events_consumed_total{queue=%q,outcome="duplicate"} 2`, queue))
		if n := count(t, pool, `SELECT count(*) FROM event_effects WHERE consumer = $1`, queue); n != 2 || calls.Load() != 2 {
			t.Fatalf("effects %d, calls %d", n, calls.Load())
		}
		if n := count(t, pool, `SELECT count(*) FROM consumed_events WHERE consumer = $1 AND source = '/forms'`, queue); n != 2 {
			t.Fatalf("records = %d", n)
		}
		emptyQueue(t, broker, queue)
	})

	t.Run("dead letters", func(t *testing.T) {
		queue := declare(t, broker, "core.dead")
		c := start(t, config, pool, queue, func(ctx context.Context, tx pgx.Tx, e events.Event) error {
			if _, err := tx.Exec(ctx, `INSERT INTO event_effects (consumer, event_id) VALUES ($1, $2)`, e.Queue, e.ID); err != nil {
				return err
			}
			switch e.ID {
			case "permanent":
				return events.Permanent(errors.New("unknown schema version"))
			case "transient":
				return errors.New("peer answered 503")
			case "panic":
				panic("handler bug")
			}
			return nil
		})
		waitFor(t, c, fmt.Sprintf(`skylab_events_consuming{queue=%q} 1`, queue))
		publish(t, broker, queue, "bad", []byte(`{"id":"bad"}`))
		publish(t, broker, queue, "permanent", cloudEvent("permanent"))
		publish(t, broker, queue, "transient", cloudEvent("transient"))
		publish(t, broker, queue, "panic", cloudEvent("panic"))
		waitFor(t, c,
			fmt.Sprintf(`skylab_events_dlq_total{queue=%q,reason="invalid_envelope"} 1`, queue),
			fmt.Sprintf(`skylab_events_dlq_total{queue=%q,reason="permanent"} 1`, queue),
			fmt.Sprintf(`skylab_events_dlq_total{queue=%q,reason="retries_exhausted"} 1`, queue),
			fmt.Sprintf(`skylab_events_dlq_total{queue=%q,reason="handler_panic"} 1`, queue),
			fmt.Sprintf(`skylab_events_retries_total{queue=%q} 3`, queue))
		// All four are in the DLQ, the main queue is empty, and nothing
		// any handler wrote stayed.
		got := map[string]bool{}
		for range 4 {
			d := get(t, broker, queue+".dlq")
			got[d.MessageId] = true
		}
		for _, id := range []string{"bad", "permanent", "transient", "panic"} {
			if !got[id] {
				t.Fatalf("DLQ holds %v, not %s", got, id)
			}
		}
		emptyQueue(t, broker, queue)
		if n := count(t, pool, `SELECT count(*) FROM event_effects WHERE consumer = $1`, queue); n != 0 {
			t.Fatalf("effects = %d", n)
		}
		if n := count(t, pool, `SELECT count(*) FROM consumed_events WHERE consumer = $1`, queue); n != 0 {
			t.Fatalf("records = %d", n)
		}
	})

	t.Run("unrelated events are acked and not recorded", func(t *testing.T) {
		queue := declare(t, broker, "core.unrelated")
		c := start(t, config, pool, queue, func(context.Context, pgx.Tx, events.Event) error {
			return fmt.Errorf("form nobody linked: %w", events.ErrUnrelated)
		})
		waitFor(t, c, fmt.Sprintf(`skylab_events_consuming{queue=%q} 1`, queue))
		publish(t, broker, queue, "u1", cloudEvent("u1"))
		waitFor(t, c, fmt.Sprintf(`skylab_events_consumed_total{queue=%q,outcome="unrelated"} 1`, queue))
		emptyQueue(t, broker, queue)
		emptyQueue(t, broker, queue+".dlq")
		if n := count(t, pool, `SELECT count(*) FROM consumed_events WHERE consumer = $1`, queue); n != 0 {
			t.Fatalf("records = %d", n)
		}
	})

	t.Run("reconnects after the broker drops the connection", func(t *testing.T) {
		queue := declare(t, broker, "core.reconnect")
		c := start(t, config, pool, queue, func(context.Context, pgx.Tx, events.Event) error { return nil })
		waitFor(t, c, "skylab_events_connected 1", fmt.Sprintf(`skylab_events_consuming{queue=%q} 1`, queue))
		broker.Ctl(t, "close_all_user_connections", coreUser, "test: dropped")
		waitFor(t, c, "skylab_events_connected 0")
		waitFor(t, c, "skylab_events_connected 1", fmt.Sprintf(`skylab_events_consuming{queue=%q} 1`, queue))
		publish(t, broker, queue, "after", cloudEvent("after"))
		waitFor(t, c, fmt.Sprintf(`skylab_events_consumed_total{queue=%q,outcome="processed"} 1`, queue))
	})

	t.Run("a missing queue does not stop the others", func(t *testing.T) {
		queue := declare(t, broker, "core.present")
		c := events.NewConsumer(config, events.NewPostgres(pool), events.Options{
			MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		})
		for _, q := range []string{"core.missing", queue} {
			if err := c.Subscribe(events.Subscription{Queue: q, Handle: func(context.Context, pgx.Tx, events.Event) error { return nil }}); err != nil {
				t.Fatal(err)
			}
		}
		run(t, c)
		waitFor(t, c, fmt.Sprintf(`skylab_events_consuming{queue=%q} 1`, queue))
		publish(t, broker, queue, "p1", cloudEvent("p1"))
		waitFor(t, c, fmt.Sprintf(`skylab_events_consumed_total{queue=%q,outcome="processed"} 1`, queue))
		if !strings.Contains(c.Prometheus(), `skylab_events_consuming{queue="core.missing"} 0`) {
			t.Fatal(c.Prometheus())
		}
		// Once ops declares it, the consumer picks it up.
		declare(t, broker, "core.missing")
		waitFor(t, c, `skylab_events_consuming{queue="core.missing"} 1`)
	})

	t.Run("a shutdown returns the deliveries in hand to the queue", func(t *testing.T) {
		queue := declare(t, broker, "core.shutdown")
		inside := make(chan struct{}, 1)
		c := events.NewConsumer(config, events.NewPostgres(pool), events.Options{})
		if err := c.Subscribe(events.Subscription{Queue: queue, Handle: func(ctx context.Context, tx pgx.Tx, e events.Event) error {
			if _, err := tx.Exec(ctx, `INSERT INTO event_effects (consumer, event_id) VALUES ($1, $2)`, e.Queue, e.ID); err != nil {
				return err
			}
			inside <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := c.Run(ctx)
		publish(t, broker, queue, "slow", cloudEvent("slow"))
		select {
		case <-inside:
		case <-time.After(30 * time.Second):
			t.Fatal("the handler never ran")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the consumer did not stop")
		}
		d := get(t, broker, queue)
		if d.MessageId != "slow" || !d.Redelivered {
			t.Fatalf("got %q redelivered %v", d.MessageId, d.Redelivered)
		}
		emptyQueue(t, broker, queue+".dlq")
		if n := count(t, pool, `SELECT count(*) FROM event_effects WHERE consumer = $1`, queue); n != 0 {
			t.Fatalf("effects = %d", n)
		}
	})

	t.Run("Keycloak's provider format", func(t *testing.T) {
		queue := declare(t, broker, "core.keycloak")
		var woken atomic.Int64
		c := events.NewConsumer(config, events.NewPostgres(pool), events.Options{Logf: t.Logf})
		if err := c.Subscribe(events.Subscription{Queue: queue, Decode: events.DecodeKeycloak, Handle: func(_ context.Context, _ pgx.Tx, e events.Event) error {
			if e.Type != "keycloak.register" {
				return events.ErrUnrelated
			}
			woken.Add(1)
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
		run(t, c)
		waitFor(t, c, fmt.Sprintf(`skylab_events_consuming{queue=%q} 1`, queue))
		// As keycloak-to-rabbitmq publishes: Keycloak's JSON, no id, no
		// message-id. The same body twice is the same event.
		body := []byte(`{"@class":"com.github.aznamier.keycloak.event.provider.EventClientNotificationMqMsg","id":null,` +
			`"time":1761213600123,"type":"REGISTER","realmId":"e-skylab","clientId":"account","userId":"u-1",` +
			`"ipAddress":"203.0.113.7","details":{"email":"someone@example.com"}}`)
		for range 2 {
			publishMessage(t, broker, queue, amqp.Publishing{
				ContentType: "application/json", DeliveryMode: amqp.Persistent, AppId: "Keycloak",
				Headers: amqp.Table{"__TypeId__": "com.github.aznamier.keycloak.event.provider.EventClientNotificationMqMsg"},
				Body:    body,
			})
		}
		waitFor(t, c,
			fmt.Sprintf(`skylab_events_consumed_total{queue=%q,outcome="processed"} 1`, queue),
			fmt.Sprintf(`skylab_events_consumed_total{queue=%q,outcome="duplicate"} 1`, queue))
		if woken.Load() != 1 {
			t.Fatalf("woken %d times", woken.Load())
		}
		if n := count(t, pool, `SELECT count(*) FROM consumed_events WHERE consumer = $1 AND source = '/keycloak' AND event_id LIKE 'sha256:%'`, queue); n != 1 {
			t.Fatalf("records = %d", n)
		}
		emptyQueue(t, broker, queue)
		emptyQueue(t, broker, queue+".dlq")
	})

	t.Run("a cancelled consumer stops its handlers while every slot is busy", func(t *testing.T) {
		queue := declare(t, broker, "core.busy")
		inside := make(chan struct{}, 3)
		stopped := make(chan struct{}, 3)
		c := events.NewConsumer(config, events.NewPostgres(pool), events.Options{
			RetryDelays: []time.Duration{time.Hour},
			MinBackoff:  50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
			Logf: t.Logf,
		})
		if err := c.Subscribe(events.Subscription{Queue: queue, Handle: func(ctx context.Context, _ pgx.Tx, _ events.Event) error {
			inside <- struct{}{}
			<-ctx.Done()
			stopped <- struct{}{}
			return ctx.Err()
		}}); err != nil {
			t.Fatal(err)
		}
		run(t, c)
		waitFor(t, c, fmt.Sprintf(`skylab_events_consuming{queue=%q} 1`, queue))
		for _, id := range []string{"b1", "b2", "b3"} {
			publish(t, broker, queue, id, cloudEvent(id))
		}
		for range events.DefaultConcurrency {
			select {
			case <-inside:
			case <-time.After(30 * time.Second):
				t.Fatal("the handlers never ran")
			}
		}
		// The third delivery is in hand, waiting for a slot. Deleting the
		// queue makes the broker cancel the consumer.
		time.Sleep(300 * time.Millisecond)
		if _, err := broker.Admin(t).QueueDelete(queue, false, false, false); err != nil {
			t.Fatal(err)
		}
		for range events.DefaultConcurrency {
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatal("the handlers kept running after the broker cancelled the consumer")
			}
		}
	})
}

// declare sets up a consumer queue as ops' topology does (SPEC §2.4), bound
// to test.events by its own name, and its DLQ.
func declare(t *testing.T, broker *testrabbitmq.Broker, queue string) string {
	t.Helper()
	admin := broker.Admin(t)
	if _, err := admin.QueueDeclare(queue+".dlq", true, false, false, false, amqp.Table{"x-queue-type": "quorum"}); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueueBind(queue+".dlq", queue, "dlx", false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.QueueDeclare(queue, true, false, false, false, amqp.Table{
		"x-queue-type":              "quorum",
		"x-delivery-limit":          5,
		"x-dead-letter-exchange":    "dlx",
		"x-dead-letter-routing-key": queue,
		"x-dead-letter-strategy":    "at-least-once",
		"x-overflow":                "reject-publish",
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueueBind(queue, queue, "test.events", false, nil); err != nil {
		t.Fatal(err)
	}
	return queue
}

func start(t *testing.T, config events.Config, pool *pgxpool.Pool, queue string, handle events.Handler) *events.Consumer {
	t.Helper()
	c := events.NewConsumer(config, events.NewPostgres(pool), events.Options{
		RetryDelays: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond},
		MinBackoff:  50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
		Logf: t.Logf,
	})
	if err := c.Subscribe(events.Subscription{Queue: queue, Handle: handle}); err != nil {
		t.Fatal(err)
	}
	run(t, c)
	return c
}

func run(t *testing.T, c *events.Consumer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := c.Run(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the consumer did not stop")
		}
	})
}

func cloudEvent(id string) []byte {
	return []byte(`{"specversion":"1.0","id":"` + id + `","source":"/forms","type":"forms.response.created",` +
		`"time":"2026-10-21T10:00:00Z","datacontenttype":"application/json",` +
		`"dataschema":"urn:skylab:schema:forms.response.created:1","data":{"formId":"f","responseId":"` + id + `"}}`)
}

// publish sends as a SKY LAB publisher does (SPEC §2.6): persistent, the
// CloudEvents content type, the event id as message-id, with confirms.
func publish(t *testing.T, broker *testrabbitmq.Broker, key, id string, body []byte) {
	t.Helper()
	publishMessage(t, broker, key, amqp.Publishing{
		ContentType: events.CloudEventsContentType, DeliveryMode: amqp.Persistent,
		MessageId: id, Type: "forms.response.created", AppId: "forms", Body: body,
	})
}

// publishMessage sends message to test.events with key, with confirms.
func publishMessage(t *testing.T, broker *testrabbitmq.Broker, key string, message amqp.Publishing) {
	t.Helper()
	admin := broker.Admin(t)
	if err := admin.Confirm(false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	confirm, err := admin.PublishWithDeferredConfirmWithContext(ctx, "test.events", key, false, false, message)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := confirm.WaitContext(ctx); err != nil || !ok {
		t.Fatalf("publish not confirmed: %v %v", ok, err)
	}
}

// get takes one message off queue, waiting for it.
func get(t *testing.T, broker *testrabbitmq.Broker, queue string) amqp.Delivery {
	t.Helper()
	admin := broker.Admin(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		d, ok, err := admin.Get(queue, true)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing arrived on %s", queue)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func emptyQueue(t *testing.T, broker *testrabbitmq.Broker, queue string) {
	t.Helper()
	admin := broker.Admin(t)
	q, err := admin.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if q.Messages != 0 {
		t.Fatalf("%s holds %d messages", queue, q.Messages)
	}
}

// waitFor waits until the consumer's metrics hold every line.
func waitFor(t *testing.T, c *events.Consumer, lines ...string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		text := c.Prometheus()
		missing := ""
		for _, line := range lines {
			if !strings.Contains(text, line+"\n") {
				missing = line
				break
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics never showed %s:\n%s", missing, text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
