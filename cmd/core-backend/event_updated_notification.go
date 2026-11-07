package main

import (
	"context"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventupdatenotify"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// eventUpdatedNotificationOn reads EVENT_UPDATED_NOTIFICATION
// (docs/event-updated-notification.md): unset or off is off, on is on.
func eventUpdatedNotificationOn(getenv func(string) string) (bool, error) {
	return onOff(getenv, "EVENT_UPDATED_NOTIFICATION")
}

// startEventUpdatedNotification starts the worker that tells an Event's
// Ticket holders of its new time or place (iletişim-servisi 06) until ctx is
// cancelled, and answers a channel that closes once it has stopped. Off, or
// without SkyMail, a change queues nothing: turning it on later does not
// tell the changes made meanwhile.
func startEventUpdatedNotification(ctx context.Context, on bool, events *event.PostgresStore, queue eventupdatenotify.Queue, tickets ticket.Store, users user.Store, sky *mail.SkyMail, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	events.NotifyUpdated(false)
	switch {
	case !on:
		logf("event updated notification: off (EVENT_UPDATED_NOTIFICATION is not on)")
		return off
	case sky == nil:
		logf("event updated notification: off (SkyMail is not configured)")
		return off
	}
	svc, err := eventupdatenotify.NewService(eventupdatenotify.Config{Queue: queue, Tickets: tickets, Events: events, Users: users, Mail: sky})
	if err != nil {
		logf("event updated notification: OFF, %v", err)
		return off
	}
	events.NotifyUpdated(true)
	logf("event updated notification: on (%s, changes gathered for %s)", mail.EventUpdatedType, event.UpdateNotificationWindow)
	return svc.Run(ctx, logf)
}
