package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/attendancemail"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// attendanceMailConfig is the door's mails' settings
// (docs/attendance-mail.md).
type attendanceMailConfig struct {
	// On is ATTENDANCE_MAIL: unset or off is off, on is on.
	On bool
	// WelcomeType is SKYMAIL_ATTENDANCE_WELCOME_TEMPLATE_KEY and
	// RecordedType SKYMAIL_ATTENDANCE_RECORDED_TEMPLATE_KEY: unset is the
	// type's own key, empty queues none of that kind.
	WelcomeType, RecordedType string
	// PerMinute is ATTENDANCE_MAIL_PER_MINUTE: unset is
	// attendancemail.DefaultPerMinute, 0 no bound.
	PerMinute int
}

// attendanceMailFromEnv reads the door's mails' settings. A value that does
// not parse stops core.
func attendanceMailFromEnv(getenv func(string) string, lookup func(string) (string, bool)) (attendanceMailConfig, error) {
	on, err := onOff(getenv, "ATTENDANCE_MAIL")
	if err != nil {
		return attendanceMailConfig{}, err
	}
	cfg := attendanceMailConfig{
		On:           on,
		WelcomeType:  templateKey(lookup, "SKYMAIL_ATTENDANCE_WELCOME_TEMPLATE_KEY", mail.AttendanceWelcomeType),
		RecordedType: templateKey(lookup, "SKYMAIL_ATTENDANCE_RECORDED_TEMPLATE_KEY", mail.AttendanceRecordedType),
		PerMinute:    attendancemail.DefaultPerMinute,
	}
	if raw := strings.TrimSpace(getenv("ATTENDANCE_MAIL_PER_MINUTE")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return attendanceMailConfig{}, fmt.Errorf("ATTENDANCE_MAIL_PER_MINUTE must be a whole number, 0 or more, not %q", raw)
		}
		cfg.PerMinute = n
	}
	return cfg, nil
}

// startAttendanceMail starts the worker that sends the door's mails until
// ctx is cancelled, and answers a channel that closes once it has stopped.
// Off, without SkyMail, or with both template keys empty, no scan queues
// anything: turned on later, it does not tell the scans of meanwhile.
func startAttendanceMail(ctx context.Context, cfg attendanceMailConfig, store *ticket.PostgresStore, queue attendancemail.Queue, events *event.PostgresStore, users user.Store, sky *mail.SkyMail, logf func(string, ...any)) <-chan struct{} {
	off := make(chan struct{})
	close(off)
	store.NotifyAttendance(false, false)
	switch {
	case !cfg.On:
		logf("attendance mail: off (ATTENDANCE_MAIL is not on)")
		return off
	case sky == nil:
		logf("attendance mail: off (SkyMail is not configured)")
		return off
	case cfg.WelcomeType == "" && cfg.RecordedType == "":
		logf("attendance mail: off (both template keys are empty)")
		return off
	}
	svc, err := attendancemail.NewService(attendancemail.Config{
		Queue: queue, Tickets: store, Events: events, Users: users, Mail: sky,
		WelcomeType: cfg.WelcomeType, RecordedType: cfg.RecordedType, PerMinute: cfg.PerMinute,
	})
	if err != nil {
		logf("attendance mail: OFF, %v", err)
		return off
	}
	store.NotifyAttendance(cfg.WelcomeType != "", cfg.RecordedType != "")
	pace := "no per-minute bound"
	if cfg.PerMinute > 0 {
		pace = strconv.Itoa(cfg.PerMinute) + "/min"
	}
	logf("attendance mail: on (welcome %s, recorded %s; %s)", orOff(cfg.WelcomeType), orOff(cfg.RecordedType), pace)
	return svc.Run(ctx, logf)
}

func orOff(key string) string {
	if key == "" {
		return "off"
	}
	return key
}
