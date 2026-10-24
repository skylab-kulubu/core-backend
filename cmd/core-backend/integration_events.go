package main

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/events"
)

// startEvents starts the Integration events side of core
// (docs/integration-events.md): the hourly cleanup of consumed_events,
// always, and with EVENTS_ENABLED=true the consumer, which connects to
// RabbitMQ in the background (a broker that is down never holds up startup or
// readiness). Off, core never connects. The consumer reads core.keycloak,
// whose events wake the user sync (wakeUserSync; nil when the sync is off,
// and the events are then acked unread). It returns what /v1/metrics shows and
// a channel that closes once both have stopped. A setting core cannot read
// stops startup, naming the variable and never its value.
func startEvents(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, wakeUserSync func()) (interface{ Prometheus() string }, <-chan struct{}) {
	config, err := events.ConfigFromEnv(getenv)
	if err != nil {
		log.Fatal(err)
	}
	store := events.NewPostgres(pool)
	cleanup := events.NewRetention(store, log.Printf)
	cleanupStopped := cleanup.Run(ctx)
	var consumer *events.Consumer
	consumerStopped := (<-chan struct{})(closedChannel())
	if config.Enabled {
		consumer = events.NewConsumer(config, store, events.Options{Logf: log.Printf})
		if err := consumer.Subscribe(keycloakEvents(wakeUserSync, log.Printf)); err != nil {
			log.Fatal(err)
		}
		if wakeUserSync == nil {
			log.Printf("integration events: %s is acked unread (the user sync is off)", keycloakQueue)
		}
		// core.forms comes here (hub ticket R13).
		consumerStopped = consumer.Run(ctx)
	}
	log.Printf("integration events: %s", config)
	done := make(chan struct{})
	go func() {
		<-consumerStopped
		<-cleanupStopped
		close(done)
	}()
	return eventsMetrics{consumer: consumer, cleanup: cleanup}, done
}

func closedChannel() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// eventsMetrics is the consumer's metrics (enabled 0 when it is off) and the
// cleanup's.
type eventsMetrics struct {
	consumer *events.Consumer
	cleanup  *events.Retention
}

func (m eventsMetrics) Prometheus() string {
	return m.consumer.Prometheus() + m.cleanup.Prometheus()
}
