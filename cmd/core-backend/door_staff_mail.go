package main

import (
	"context"

	"github.com/skylab-kulubu/core-backend/internal/doorstaffmail"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// doorStaffMailOn reads DOOR_STAFF_MAIL (docs/door-staff-mail.md): unset or
// off is off, on is on.
func doorStaffMailOn(getenv func(string) string) (bool, error) {
	return onOff(getenv, "DOOR_STAFF_MAIL")
}

// startDoorStaffMail starts the worker that tells a person added to an
// Event's door staff until ctx is cancelled, and answers a channel that
// closes once it has stopped. Off, or without SkyNotify, adding a person
// queues nothing: turning it on later tells none of the people added
// meanwhile.
func startDoorStaffMail(ctx context.Context, on bool, events *event.PostgresStore, queue doorstaffmail.Queue, users user.Store, sky *mail.SkyNotify, panelURL string, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	events.NotifyDoorStaff(false)
	switch {
	case !on:
		logf("door staff mail: off (DOOR_STAFF_MAIL is not on)")
		return off
	case sky == nil:
		logf("door staff mail: off (SkyNotify is not configured)")
		return off
	}
	svc, err := doorstaffmail.NewService(doorstaffmail.Config{Queue: queue, Events: events, Users: users, Mail: sky, PanelURL: panelURL})
	if err != nil {
		logf("door staff mail: OFF, %v", err)
		return off
	}
	events.NotifyDoorStaff(true)
	link := "no door link (PANEL_URL unset)"
	if panelURL != "" {
		link = "door link on the panel"
	}
	logf("door staff mail: on (%s, after %s; %s)", mail.DoorStaffAddedType, event.DoorStaffMailDelay, link)
	return svc.Run(ctx, logf)
}
