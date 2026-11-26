package handlers

import (
	"testing"
)

func TestDoorQRLimitsFromEnv(t *testing.T) {
	t.Parallel()
	env := func(values map[string]string) func(string) string {
		return func(k string) string { return values[k] }
	}
	got, err := DoorQRLimitsFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultDoorQRLimits() || got.GuestFailuresPerClient != 300 || got.GuestFailuresPerEmail != 10 {
		t.Fatalf("defaults %+v", got)
	}
	got, err = DoorQRLimitsFromEnv(env(map[string]string{
		GuestFailuresPerIPEnv: " 1000 ", GuestFailuresPerEmailEnv: "5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.GuestFailuresPerClient != 1000 || got.GuestFailuresPerEmail != 5 || got.MintsPerSubject != 30 {
		t.Fatalf("set %+v", got)
	}
	for _, bad := range []map[string]string{
		{GuestFailuresPerIPEnv: "0"},
		{GuestFailuresPerIPEnv: "-1"},
		{GuestFailuresPerIPEnv: "300/m"},
		{GuestFailuresPerIPEnv: "100001"},
		{GuestFailuresPerEmailEnv: "ten"},
	} {
		if _, err := DoorQRLimitsFromEnv(env(bad)); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

func TestGuestCheckInSubject(t *testing.T) {
	t.Parallel()
	a := guestCheckInSubject([]byte(`{"email":"Ada@Example.com "}`))
	if b := guestCheckInSubject([]byte(`{"email":" ada@example.com","doorToken":"x"}`)); a != b {
		t.Fatalf("same e-mail, case and spaces aside: %s %s", a, b)
	}
	if b := guestCheckInSubject([]byte(`{"email":"grace@example.com"}`)); a == b {
		t.Fatal("other e-mail shares the subject")
	}
	// The key is what the handler uses when both are sent.
	k := guestCheckInSubject([]byte(`{"email":"ada@example.com","ticketKey":"k1"}`))
	if k == a || k != guestCheckInSubject([]byte(`{"ticketKey":" k1 "}`)) {
		t.Fatal("ticket key subject")
	}
	if guestCheckInSubject([]byte(`not json`)) != guestCheckInSubject(nil) {
		t.Fatal("unparsed bodies share one subject")
	}
}
