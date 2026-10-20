package usersync

import (
	"testing"
	"time"
)

func TestConfigFromEnv(t *testing.T) {
	t.Parallel()
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	got, err := ConfigFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	istanbul := time.FixedZone("+03", 3*60*60)
	if !got.Enabled || got.Interval != 5*time.Minute || !got.WelcomeSince.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, istanbul)) {
		t.Fatalf("defaults %+v", got)
	}
	got, err = ConfigFromEnv(env(map[string]string{EnabledEnv: "false", IntervalEnv: "10m", WelcomeSinceEnv: "2026-10-20T12:00:00+03:00"}))
	if err != nil || got.Enabled || got.Interval != 10*time.Minute || !got.WelcomeSince.Equal(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("set %+v %v", got, err)
	}
	for _, bad := range []map[string]string{
		{EnabledEnv: "yes"}, {IntervalEnv: "30s"}, {IntervalEnv: "48h"}, {IntervalEnv: "soon"}, {WelcomeSinceEnv: "2026-10-19"},
	} {
		if _, err := ConfigFromEnv(env(bad)); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}
