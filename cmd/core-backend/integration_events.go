package main

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

// startEvents starts the Integration events side of core
// (docs/integration-events.md): the hourly cleanup of consumed_events,
// always, and with EVENTS_ENABLED=true the consumer, which connects to
// RabbitMQ in the background (a broker that is down never holds up startup or
// readiness). Off, core never connects. The consumer reads core.keycloak,
// whose events wake the user sync and the communication preference cleanup
// and remove a person Keycloak deleted outside the erasure saga
// (keycloakEvents; a part that is off acks its events unread). With forms
// (EVENTS_FORMS_ENABLED=true) it reads core.forms too, whose events write
// Tickets, mark feedback invites answered and close feedback forms' reminders
// by the HTTP report's rule (formsfeed). The hourly cleanup of
// forms_record_versions runs always, as consumed_events'. It returns what
// /v1/metrics shows and a channel that closes once all have stopped. A
// setting core cannot read stops startup, naming the variable and never its
// value.
func startEvents(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, keycloak *keycloakHandling, forms *formsfeed.Recorder) (interface{ Prometheus() string }, <-chan struct{}) {
	config, err := events.ConfigFromEnv(getenv)
	if err != nil {
		log.Fatal(err)
	}
	store := events.NewPostgres(pool)
	cleanup := events.NewRetention(store, log.Printf)
	cleanupStopped := cleanup.Run(ctx)
	versionsStopped := formsfeed.RunVersionCleanup(ctx, pool, log.Printf)
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
		if forms != nil {
			if err := consumer.Subscribe(formsfeed.Subscription(forms)); err != nil {
				log.Fatal(err)
			}
			log.Printf("integration events: %s on (%s=true)", formsfeed.Queue, formsEventsEnv)
		} else {
			log.Printf("integration events: %s off (%s=false); its events wait in the queue", formsfeed.Queue, formsEventsEnv)
		}
		consumerStopped = consumer.Run(ctx)
	} else if forms != nil {
		log.Printf("integration events: %s=true does nothing while %s=false", formsEventsEnv, events.EnabledEnv)
	}
	log.Printf("integration events: %s", config)
	done := make(chan struct{})
	go func() {
		<-consumerStopped
		<-cleanupStopped
		<-versionsStopped
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
// deleted outside the erasure saga. core.forms' counters are formsMetrics'
// (the pulls share its recorder).
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
