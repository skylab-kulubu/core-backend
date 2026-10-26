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
// whose events wake the user sync and the communication preference cleanup
// and remove a person Keycloak deleted outside the erasure saga
// (keycloakEvents; a part that is off acks its events unread). It returns
// what /v1/metrics shows and a channel that closes once both have stopped. A
// setting core cannot read stops startup, naming the variable and never its
// value.
func startEvents(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, keycloak *keycloakHandling) (interface{ Prometheus() string }, <-chan struct{}) {
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
		if keycloak.logf == nil {
			keycloak.logf = log.Printf
		}
		if err := consumer.Subscribe(keycloakEvents(keycloak)); err != nil {
			log.Fatal(err)
		}
		log.Printf("integration events: %s %s", keycloakQueue, keycloak)
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
	metrics := eventsMetrics{consumer: consumer, cleanup: cleanup}
	if consumer != nil {
		metrics.keycloak = keycloak
	}
	return metrics, done
}

func closedChannel() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// eventsMetrics is the consumer's metrics (enabled 0 when it is off), the
// cleanup's and, when the consumer is on, core.keycloak's count of people
// deleted outside the erasure saga.
type eventsMetrics struct {
	consumer *events.Consumer
	cleanup  *events.Retention
	keycloak *keycloakHandling
}

func (m eventsMetrics) Prometheus() string {
	out := m.consumer.Prometheus() + m.cleanup.Prometheus()
	if m.keycloak != nil {
		out += m.keycloak.prometheus()
	}
	return out
}
