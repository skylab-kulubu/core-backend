package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/testrabbitmq"
)

// Keycloak's deletions end to end, once: the provider's GROUP.DELETE and
// USER DELETE on a real RabbitMQ with ops' topology, core.keycloak's
// consumer and handler on a real database. The deleted team's and the
// deleted person's preferences go; the saga's person keeps theirs; a repeat
// of the event has no second effect.
func TestKeycloakDeletionsThroughTheBroker(t *testing.T) {
	broker := testrabbitmq.Start(t)
	broker.AddUser(t, "core_test", "core-test-password", "^$", "^$", `^core\.`)
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	admin := broker.Admin(t)
	for _, step := range []func() error{
		func() error { return admin.ExchangeDeclare("keycloak.events", "topic", true, false, false, false, nil) },
		func() error { return admin.ExchangeDeclare("dlx", "direct", true, false, false, false, nil) },
		func() error {
			_, err := admin.QueueDeclare(keycloakQueue+".dlq", true, false, false, false, amqp.Table{"x-queue-type": "quorum"})
			return err
		},
		func() error {
			_, err := admin.QueueDeclare(keycloakQueue, true, false, false, false, amqp.Table{
				"x-queue-type": "quorum", "x-delivery-limit": 5,
				"x-dead-letter-exchange": "dlx", "x-dead-letter-routing-key": keycloakQueue,
			})
			return err
		},
		func() error {
			return admin.QueueBind(keycloakQueue, "KK.EVENT.ADMIN.e-skylab.SUCCESS.USER.*", "keycloak.events", false, nil)
		},
		func() error {
			return admin.QueueBind(keycloakQueue, "KK.EVENT.ADMIN.e-skylab.SUCCESS.GROUP.DELETE", "keycloak.events", false, nil)
		},
		// A binding too wide: another realm's deletion reaches the queue.
		func() error {
			return admin.QueueBind(keycloakQueue, "KK.EVENT.ADMIN.e-skylab-sandbox.SUCCESS.USER.DELETE", "keycloak.events", false, nil)
		},
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}

	// Keycloak: the members' root, two teams and a sub-team; three people.
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "g-root", Name: "UYELER", Path: authz.MembersRoot})
	dir.PutGroup(identity.Group{ID: goneGroup, Name: "WEBLAB", Path: authz.MembersRoot + "/WEBLAB"})
	dir.PutGroup(identity.Group{ID: "g-front", Name: "FRONT", Path: authz.MembersRoot + "/WEBLAB/FRONT"})
	store := commpref.NewPostgresStore(pool)
	gone, sagas, member := uuid.MustParse(gonePerson), uuid.New(), uuid.New()
	on := true
	for _, row := range []struct {
		person uuid.UUID
		team   string
	}{{gone, "g-airlab"}, {sagas, "g-airlab"}, {member, goneGroup}, {member, "g-front"}, {member, "g-airlab"}} {
		if err := store.Apply(ctx, row.person, []commpref.Change{{Channel: commpref.ChannelEmail, Category: commpref.Team(row.team, commpref.KindNews), Enabled: &on}}, commpref.SourceAccountCenter, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, person := range []uuid.UUID{gone, sagas, member} {
		dir.PutUser(identity.Person{ID: person})
	}
	// The saga's person: an active account whose erasure was asked for
	// after their choice.
	for _, sql := range []string{
		`INSERT INTO users (id, email) VALUES ($1, '')`,
		`INSERT INTO account_deletion_requests (id, subject_id) VALUES (gen_random_uuid(), $1)`,
	} {
		if _, err := pool.Exec(ctx, sql, sagas); err != nil {
			t.Fatal(err)
		}
	}
	airlab := identity.Group{ID: "g-airlab", Name: "AIRLAB", Path: authz.MembersRoot + "/AIRLAB"}
	dir.PutGroup(airlab)

	preferences := commpref.NewService(commpref.Config{}, store, dir, authz.DefaultPrivilegedRoots())
	preferences.SetTeamCleanupGap(100 * time.Millisecond)
	runCtx, cancel := context.WithCancel(ctx)
	cleanupStopped := commpref.MaintainCleanup(runCtx, preferences, time.Hour, nil)
	var logMu sync.Mutex
	var logs []string
	h := &keycloakHandling{
		realm: keycloakRoutingRealm("e-skylab"), people: dir, requested: erasureRequested, erase: commpref.EraseSubjectIn,
		wakeTeams: preferences.WakeTeamCleanup,
		logf: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	}
	consumer := events.NewConsumer(events.Config{
		Enabled: true, Host: broker.Host, Port: broker.Port, Vhost: testrabbitmq.Vhost,
		Username: "core_test", Password: "core-test-password",
	}, events.NewPostgres(pool), events.Options{Logf: t.Logf, MinBackoff: 50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond})
	if err := consumer.Subscribe(keycloakEvents(h)); err != nil {
		t.Fatal(err)
	}
	consumerStopped := consumer.Run(runCtx)
	t.Cleanup(func() {
		cancel()
		<-consumerStopped
		<-cleanupStopped
	})
	rows := func(person uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM communication_preferences WHERE user_id = $1`, person).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitUntil := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("%s; metrics:\n%s", what, consumer.Prometheus())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitUntil("not consuming", func() bool {
		return strings.Contains(consumer.Prometheus(), `skylab_events_consuming{queue="core.keycloak"} 1`)
	})
	if rows(gone) != 1 || rows(sagas) != 1 || rows(member) != 3 {
		t.Fatalf("the first cleanup removed rows: %d %d %d", rows(gone), rows(sagas), rows(member))
	}

	// The provider's messages (e-skylab-keycloak EventMessages): the body
	// of ids, the event id as message-id, the routing key with the realm's
	// name.
	publish := func(key, id, body string) {
		t.Helper()
		if err := admin.Confirm(false); err != nil {
			t.Fatal(err)
		}
		confirm, err := admin.PublishWithDeferredConfirmWithContext(ctx, "keycloak.events", key, false, false, amqp.Publishing{
			ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: id, Body: []byte(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := confirm.WaitContext(ctx); err != nil || !ok {
			t.Fatalf("publish not confirmed: %v %v", ok, err)
		}
	}
	// The Group goes in Keycloak, its sub-Group with it, and only the Group
	// has an event: both teams' rows go, the other team's stays.
	if err := dir.DeleteGroup(ctx, goneGroup); err != nil {
		t.Fatal(err)
	}
	publish(groupDelete, "ev-2", adminGroupDelete(goneGroup))
	waitUntil("the deleted teams' rows stay", func() bool { return rows(member) == 1 })

	// Two people go from Keycloak: one outside the saga, whose event comes
	// twice, and the saga's.
	for _, person := range []uuid.UUID{gone, sagas} {
		if err := dir.DeleteUser(ctx, person); err != nil {
			t.Fatal(err)
		}
	}
	publish(adminDelete, "ev-1", adminUserDelete(gonePerson))
	publish(adminDelete, "ev-1", adminUserDelete(gonePerson))
	publish(adminDelete, "ev-4", strings.Replace(adminUserDelete(sagas.String()), `"ev-1"`, `"ev-4"`, 1))
	waitUntil("not every event handled", func() bool {
		m := consumer.Prometheus()
		return strings.Contains(m, `skylab_events_consumed_total{queue="core.keycloak",outcome="processed"} 3`) &&
			strings.Contains(m, `skylab_events_consumed_total{queue="core.keycloak",outcome="duplicate"} 1`)
	})
	if rows(gone) != 0 || rows(sagas) != 1 {
		t.Fatalf("rows: deleted person %d, the saga's %d", rows(gone), rows(sagas))
	}
	var records int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM consumed_events WHERE consumer = $1 AND source = '/keycloak'`, keycloakQueue).Scan(&records); err != nil || records != 3 {
		t.Fatalf("records %d %v", records, err)
	}
	// Another realm's deletion of a person this realm no longer has either
	// is unrelated: the realm is the routing key's.
	if err := dir.DeleteUser(ctx, member); err != nil {
		t.Fatal(err)
	}
	publish("KK.EVENT.ADMIN.e-skylab-sandbox.SUCCESS.USER.DELETE", "ev-5", strings.Replace(adminUserDelete(member.String()), `"ev-1"`, `"ev-5"`, 1))
	waitUntil("the other realm's event not acked", func() bool {
		return strings.Contains(consumer.Prometheus(), `skylab_events_consumed_total{queue="core.keycloak",outcome="unrelated"} 1`)
	})
	if rows(member) != 1 {
		t.Fatalf("another realm's event removed rows: %d", rows(member))
	}
	logMu.Lock()
	defer logMu.Unlock()
	if h.outsideErasure.Load() != 1 || len(logs) != 1 || !strings.Contains(logs[0], `"event":"keycloak_user_deleted_outside_erasure"`) || !strings.Contains(logs[0], gonePerson) {
		t.Fatalf("counted %d, logs %q", h.outsideErasure.Load(), logs)
	}
}
