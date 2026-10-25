package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketnotify"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// ticketCreatedNotificationOn reads TICKET_CREATED_NOTIFICATION
// (docs/ticket-created-notification.md): unset or off is off, on is on.
func ticketCreatedNotificationOn(getenv func(string) string) (bool, error) {
	switch raw := strings.TrimSpace(getenv("TICKET_CREATED_NOTIFICATION")); raw {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("TICKET_CREATED_NOTIFICATION must be on or off, not %q", raw)
	}
}

// startTicketCreatedNotification starts the worker that tells a new Ticket's
// holder (iletişim-servisi 06) until ctx is cancelled, and answers a channel
// that closes once it has stopped. Off, or without SkyMail, a new Ticket
// queues nothing: turning it on later does not tell the Tickets written
// meanwhile.
func startTicketCreatedNotification(ctx context.Context, on bool, store *ticket.PostgresStore, queue ticketnotify.Queue, events *event.PostgresStore, users user.Store, sky *mail.SkyMail, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	store.NotifyCreated(false)
	switch {
	case !on:
		logf("ticket notification: off (TICKET_CREATED_NOTIFICATION is not on)")
		return off
	case sky == nil:
		logf("ticket notification: off (SkyMail is not configured)")
		return off
	}
	svc, err := ticketnotify.NewService(ticketnotify.Config{Queue: queue, Tickets: store, Events: events, Users: users, Mail: sky})
	if err != nil {
		logf("ticket notification: OFF, %v", err)
		return off
	}
	store.NotifyCreated(true)
	logf("ticket notification: on (%s)", mail.TicketCreatedType)
	return svc.Run(ctx, logf)
}
