package main

import "testing"

func TestEventChangedNotificationOn(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "off": false, " on ": true, "on": true} {
		got, err := eventChangedNotificationOn(func(name string) string {
			if name != "EVENT_CHANGED_NOTIFICATION" {
				t.Fatalf("read %s", name)
			}
			return raw
		})
		if err != nil || got != want {
			t.Fatalf("%q: %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := eventChangedNotificationOn(func(string) string { return "true" }); err == nil {
		t.Fatal("true must be refused: the value is on or off")
	}
}
