package main

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/eventreminder"
)

func TestEventReminderFromEnv(t *testing.T) {
	env := func(vars map[string]string) (func(string) string, func(string) (string, bool)) {
		return func(name string) string { return vars[name] }, func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		}
	}
	for name, tc := range map[string]struct {
		vars map[string]string
		want eventReminderConfig
	}{
		"unset":       {map[string]string{}, eventReminderConfig{Type: "event.reminder", PerMinute: eventreminder.DefaultPerMinute}},
		"on":          {map[string]string{"EVENT_REMINDER_NOTIFICATION": " on "}, eventReminderConfig{On: true, Type: "event.reminder", PerMinute: 20}},
		"own key":     {map[string]string{"EVENT_REMINDER_NOTIFICATION": "on", "SKYMAIL_EVENT_REMINDER_TEMPLATE_KEY": " club.reminder "}, eventReminderConfig{On: true, Type: "club.reminder", PerMinute: 20}},
		"empty key":   {map[string]string{"EVENT_REMINDER_NOTIFICATION": "on", "SKYMAIL_EVENT_REMINDER_TEMPLATE_KEY": ""}, eventReminderConfig{On: true, PerMinute: 20}},
		"own pace":    {map[string]string{"EVENT_REMINDER_PER_MINUTE": "10"}, eventReminderConfig{Type: "event.reminder", PerMinute: 10}},
		"no bound":    {map[string]string{"EVENT_REMINDER_PER_MINUTE": "0"}, eventReminderConfig{Type: "event.reminder"}},
		"off spelled": {map[string]string{"EVENT_REMINDER_NOTIFICATION": "off"}, eventReminderConfig{Type: "event.reminder", PerMinute: 20}},
	} {
		got, err := eventReminderFromEnv(env(tc.vars))
		if err != nil || got != tc.want {
			t.Fatalf("%s: %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
	for _, bad := range []map[string]string{
		{"EVENT_REMINDER_NOTIFICATION": "true"},
		{"EVENT_REMINDER_PER_MINUTE": "-1"},
		{"EVENT_REMINDER_PER_MINUTE": "twenty"},
	} {
		if _, err := eventReminderFromEnv(env(bad)); err == nil {
			t.Fatalf("%v must be refused", bad)
		}
	}
}
