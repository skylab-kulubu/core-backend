package main

import (
	"context"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventstatusmail"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// startEventStatusMail starts the worker that sends the Event status mails
// (docs/event-cancel.md, ticket 14) until ctx is cancelled, and answers a
// channel that closes once it has stopped. Without SkyMail it is off; the
// cancellation still queues its mail, sent once SkyMail is configured. A
// mail whose template key is empty is done with unsent.
func startEventStatusMail(ctx context.Context, events *event.PostgresStore, tickets ticket.Store, users user.Store, sky *mail.SkyMail, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	if sky == nil {
		logf("event status mail: off (SkyMail is not configured)")
		return off
	}
	svc, err := eventstatusmail.NewService(eventstatusmail.Config{Queue: events, Events: events, Tickets: tickets, Users: users, Mail: sky})
	if err != nil {
		logf("event status mail: OFF, %v", err)
		return off
	}
	keyOr := func(kind mail.EventStatusKind) string {
		if key := sky.EventStatusTemplateKey(kind); key != "" {
			return key
		}
		return "off"
	}
	logf("event status mail: on (cancelled %s; reinstated %s)", keyOr(mail.EventStatusCancelled), keyOr(mail.EventStatusReinstated))
	return svc.Run(ctx, logf)
}
