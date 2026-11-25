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

// eventChangedNotificationOn reads EVENT_CHANGED_NOTIFICATION
// (docs/event-updated-notification.md#other-details): unset or off is off,
// on is on.
func eventChangedNotificationOn(getenv func(string) string) (bool, error) {
	return onOff(getenv, "EVENT_CHANGED_NOTIFICATION")
}

// startEventUpdatedNotification starts the worker that tells an Event's
// Ticket holders of its new time or place (iletişim-servisi 06) until ctx is
// cancelled, and answers a channel that closes once it has stopped. Off, or
// without SkyNotify, a change queues nothing: turning it on later does not
// tell the changes made meanwhile.
func startEventUpdatedNotification(ctx context.Context, on, changedOn bool, events *event.PostgresStore, queue eventupdatenotify.Queue, tickets ticket.Store, users user.Store, sky *mail.SkyNotify, page ticket.PageConfig, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	events.NotifyUpdated(false)
	events.NotifyChanged(false)
	switch {
	case !on && !changedOn:
		logf("event updated notification: off (EVENT_UPDATED_NOTIFICATION and EVENT_CHANGED_NOTIFICATION are not on)")
		return off
	case sky == nil:
		logf("event updated notification: off (SkyNotify is not configured)")
		return off
	}
	svc, err := eventupdatenotify.NewService(eventupdatenotify.Config{Queue: queue, Tickets: tickets, Events: events, Users: users, Mail: sky, Page: page, Changed: changedOn})
	if err != nil {
		logf("event updated notification: OFF, %v", err)
		return off
	}
	events.NotifyUpdated(on)
	events.NotifyChanged(changedOn)
	logf("event updated notification: time and place %s, other details %s (%s, %s; changes gathered for %s)",
		onWord(on), onWord(changedOn), mail.EventUpdatedType, mail.EventChangedType, event.UpdateNotificationWindow)
	return svc.Run(ctx, logf)
}

func onWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
