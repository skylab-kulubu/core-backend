package main

import "testing"

func TestTicketCreatedNotificationOn(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "off": false, " on ": true, "on": true} {
		got, err := ticketCreatedNotificationOn(func(string) string { return raw })
		if err != nil || got != want {
			t.Fatalf("%q: %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := ticketCreatedNotificationOn(func(string) string { return "true" }); err == nil {
		t.Fatal("true must be refused: the value is on or off")
	}
}
