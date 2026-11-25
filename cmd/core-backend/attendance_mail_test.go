package main

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/attendancemail"
)

func TestAttendanceMailFromEnv(t *testing.T) {
	env := func(vars map[string]string) (func(string) string, func(string) (string, bool)) {
		return func(name string) string { return vars[name] }, func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		}
	}
	const welcome, recorded = "event.attendance-welcome", "event.attendance-recorded"
	for name, tc := range map[string]struct {
		vars map[string]string
		want attendanceMailConfig
	}{
		"unset is off": {map[string]string{}, attendanceMailConfig{WelcomeType: welcome, RecordedType: recorded, PerMinute: attendancemail.DefaultPerMinute}},
		"on":           {map[string]string{"ATTENDANCE_MAIL": " on "}, attendanceMailConfig{On: true, WelcomeType: welcome, RecordedType: recorded, PerMinute: 20}},
		"off spelled":  {map[string]string{"ATTENDANCE_MAIL": "off"}, attendanceMailConfig{WelcomeType: welcome, RecordedType: recorded, PerMinute: 20}},
		"own key":      {map[string]string{"SKYNOTIFY_ATTENDANCE_WELCOME_TEMPLATE_KEY": " club.welcome "}, attendanceMailConfig{WelcomeType: "club.welcome", RecordedType: recorded, PerMinute: 20}},
		"no receipts":  {map[string]string{"SKYNOTIFY_ATTENDANCE_RECORDED_TEMPLATE_KEY": ""}, attendanceMailConfig{WelcomeType: welcome, PerMinute: 20}},
		"own pace":     {map[string]string{"ATTENDANCE_MAIL_PER_MINUTE": "10"}, attendanceMailConfig{WelcomeType: welcome, RecordedType: recorded, PerMinute: 10}},
		"no bound":     {map[string]string{"ATTENDANCE_MAIL_PER_MINUTE": "0"}, attendanceMailConfig{WelcomeType: welcome, RecordedType: recorded}},
	} {
		got, err := attendanceMailFromEnv(env(tc.vars))
		if err != nil || got != tc.want {
			t.Fatalf("%s: %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
	for _, bad := range []map[string]string{
		{"ATTENDANCE_MAIL": "true"},
		{"ATTENDANCE_MAIL_PER_MINUTE": "-1"},
		{"ATTENDANCE_MAIL_PER_MINUTE": "twenty"},
	} {
		if _, err := attendanceMailFromEnv(env(bad)); err == nil {
			t.Fatalf("%v must be refused", bad)
		}
	}
}
