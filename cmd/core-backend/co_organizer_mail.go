package main

import (
	"context"

	"github.com/skylab-kulubu/core-backend/internal/coorganizermail"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// coOrganizerMailOn reads CO_ORGANIZER_MAIL (docs/co-organizers.md#mails):
// unset or off is off, on is on.
func coOrganizerMailOn(getenv func(string) string) (bool, error) {
	return onOff(getenv, "CO_ORGANIZER_MAIL")
}

// startCoOrganizerMail starts the worker that tells the people of a change
// of an Event's Co-organizers until ctx is cancelled, and answers a channel
// that closes once it has stopped. Off, or without SkyNotify, a change
// queues nothing: turning it on later tells none of the changes made
// meanwhile.
func startCoOrganizerMail(ctx context.Context, on bool, events *event.PostgresStore, queue coorganizermail.Queue, users user.Store,
	leaders coorganizermail.Leaders, sky *mail.SkyNotify, panelURL string, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	events.NotifyCoOrganizers(false)
	switch {
	case !on:
		logf("co-organizer mail: off (CO_ORGANIZER_MAIL is not on)")
		return off
	case sky == nil:
		logf("co-organizer mail: off (SkyNotify is not configured)")
		return off
	}
	svc, err := coorganizermail.NewService(coorganizermail.Config{Queue: queue, Events: events, Users: users, Leaders: leaders, Mail: sky, PanelURL: panelURL})
	if err != nil {
		logf("co-organizer mail: OFF, %v", err)
		return off
	}
	events.NotifyCoOrganizers(true)
	link := "no panel link (PANEL_URL unset)"
	if panelURL != "" {
		link = "panel link on"
	}
	logf("co-organizer mail: on (%s, %s, after %s; %s)", mail.CoOrganizerAddedType, mail.CoOrganizerRemovedType, event.CoOrganizerMailDelay, link)
	return svc.Run(ctx, logf)
}
