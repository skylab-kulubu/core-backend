package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventreminder"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// eventReminderConfig is the reminder's settings (docs/event-reminder.md).
type eventReminderConfig struct {
	// On is EVENT_REMINDER_NOTIFICATION: unset or off is off, on is on.
	On bool
	// Type is SKYNOTIFY_EVENT_REMINDER_TEMPLATE_KEY: unset is event.reminder,
	// empty plans nothing.
	Type string
	// PerMinute is EVENT_REMINDER_PER_MINUTE: unset is
	// eventreminder.DefaultPerMinute, 0 no bound.
	PerMinute int
}

// eventReminderFromEnv reads the reminder's settings. A value that does not
// parse stops core.
func eventReminderFromEnv(getenv func(string) string, lookup func(string) (string, bool)) (eventReminderConfig, error) {
	on, err := onOff(getenv, "EVENT_REMINDER_NOTIFICATION")
	if err != nil {
		return eventReminderConfig{}, err
	}
	cfg := eventReminderConfig{On: on, Type: templateKey(lookup, "SKYNOTIFY_EVENT_REMINDER_TEMPLATE_KEY", mail.EventReminderType), PerMinute: eventreminder.DefaultPerMinute}
	if raw := strings.TrimSpace(getenv("EVENT_REMINDER_PER_MINUTE")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return eventReminderConfig{}, fmt.Errorf("EVENT_REMINDER_PER_MINUTE must be a whole number, 0 or more, not %q", raw)
		}
		cfg.PerMinute = n
	}
	return cfg, nil
}

// startEventReminder starts the worker that plans and sends the reminder
// the day before an Event until ctx is cancelled, and answers a channel that
// closes once it has stopped. Off, without SkyNotify, or with an empty
// template key, nothing is planned: turned on later, it plans the Events of
// the next day from that evening's 18:00 on.
func startEventReminder(ctx context.Context, cfg eventReminderConfig, queue eventreminder.Queue, events *event.PostgresStore, tickets ticket.Store, users user.Store, sky *mail.SkyNotify, page ticket.PageConfig, apiOrigin string, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	switch {
	case !cfg.On:
		logf("event reminder: off (EVENT_REMINDER_NOTIFICATION is not on)")
		return off
	case sky == nil:
		logf("event reminder: off (SkyNotify is not configured)")
		return off
	case cfg.Type == "":
		logf("event reminder: off (SKYNOTIFY_EVENT_REMINDER_TEMPLATE_KEY is empty)")
		return off
	}
	svc, err := eventreminder.NewService(eventreminder.Config{
		Queue: queue, Tickets: tickets, Events: events, Users: users, Mail: sky,
		Type: cfg.Type, Page: page, APIOrigin: apiOrigin, PerMinute: cfg.PerMinute,
	})
	if err != nil {
		logf("event reminder: OFF, %v", err)
		return off
	}
	pace := "no per-minute bound"
	if cfg.PerMinute > 0 {
		pace = strconv.Itoa(cfg.PerMinute) + "/min"
	}
	logf("event reminder: on (%s, %02d:00 Europe/Istanbul, %s)", cfg.Type, eventreminder.PlanHour, pace)
	return svc.Run(ctx, logf)
}
