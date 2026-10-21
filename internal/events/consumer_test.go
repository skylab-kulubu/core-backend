package events

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
)

func TestBackoffDoublesToItsBound(t *testing.T) {
	b := backoff{min: time.Second, max: 8 * time.Second}
	var got []time.Duration
	for range 6 {
		d := b.next()
		got = append(got, b.current)
		if d < b.current || d > b.current+b.current/5 {
			t.Fatalf("wait %s outside %s..+20%%", d, b.current)
		}
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	}
	b.reset()
	if b.next(); b.current != time.Second {
		t.Fatalf("after reset: %s", b.current)
	}
}

// A broker that cannot be reached holds nothing up: Run returns at once,
// keeps trying with a growing wait, says so in the metrics and the log
// (never with the password), and stops as soon as it is told.
func TestRunWithoutABroker(t *testing.T) {
	// A port nobody listens on: listen, take its number, close.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	var logs atomic.Value
	logs.Store("")
	c := NewConsumer(Config{Enabled: true, Host: "127.0.0.1", Port: port, Vhost: "skylab", Username: "core_a", Password: "the-password"}, &memoryStore{}, Options{
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		Logf: func(format string, args ...any) { logs.Store(logs.Load().(string) + format + "\n") },
	})
	if err := c.Subscribe(Subscription{Queue: "core.forms", Handle: func(context.Context, pgx.Tx, Event) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	done := c.Run(ctx)
	if time.Since(started) > time.Second {
		t.Fatal("Run blocked")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(c.Prometheus(), "skylab_events_connect_failures_total 3\n") {
		if time.Now().After(deadline) {
			t.Fatalf("no third failure:\n%s", c.Prometheus())
		}
		time.Sleep(5 * time.Millisecond)
	}
	text := c.Prometheus()
	for _, line := range []string{"skylab_events_enabled 1", "skylab_events_connected 0", `skylab_events_consuming{queue="core.forms"} 0`} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("metrics lack %s:\n%s", line, text)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if !strings.Contains(logs.Load().(string), "events_connect_failed") {
		t.Fatalf("log = %s", logs.Load())
	}
}

// A dial that hangs does not hold up a shutdown.
func TestRunStopsDuringADial(t *testing.T) {
	c := NewConsumer(Config{Enabled: true}, &memoryStore{}, Options{})
	release := make(chan struct{})
	defer close(release)
	c.dial = func(Config) (*amqp.Connection, error) {
		<-release
		return nil, errors.New("late")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := c.Run(ctx)
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run waited for the dial")
	}
}

func TestRedact(t *testing.T) {
	c := NewConsumer(Config{Password: "p4ss"}, &memoryStore{}, Options{})
	if got := c.redact(errors.New("auth p4ss failed")); got != "auth [redacted] failed" {
		t.Fatal(got)
	}
	if c.redact(nil) != "" {
		t.Fatal("nil error")
	}
}

func TestPrometheusOff(t *testing.T) {
	var c *Consumer
	if got := c.Prometheus(); got != "# TYPE skylab_events_enabled gauge\nskylab_events_enabled 0\n" {
		t.Fatal(got)
	}
}
