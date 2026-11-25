package main

import "testing"

func TestDoorStaffMailOn(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "off": false, " on ": true, "on": true} {
		got, err := doorStaffMailOn(func(name string) string {
			if name != "DOOR_STAFF_MAIL" {
				t.Fatalf("read %s", name)
			}
			return raw
		})
		if err != nil || got != want {
			t.Fatalf("%q: %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := doorStaffMailOn(func(string) string { return "true" }); err == nil {
		t.Fatal("true must be refused: the value is on or off")
	}
}
