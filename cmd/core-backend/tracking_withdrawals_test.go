package main

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/commpref"
)

// The withdrawal worker sends only with a readable internal address and
// core's client credentials. An address that does not read (the erasure
// fallback is read even while the erasure worker is off) warns and leaves
// the sender off; it never stops core.
func TestNewTrackingWithdrawals(t *testing.T) {
	creds := map[string]string{"KEYCLOAK_CLIENT_ID": "core", "KEYCLOAK_CLIENT_SECRET": "s3cret"}
	for name, c := range map[string]struct {
		env      map[string]string
		sending  bool
		fromWant string
	}{
		"own address":        {map[string]string{"COMMUNICATION_TRACKING_WITHDRAWAL_URL": "http://skymail:8080"}, true, "COMMUNICATION_TRACKING_WITHDRAWAL_URL"},
		"erasure fallback":   {map[string]string{"ACCOUNT_ERASURE_SKYMAIL_URL": "http://skymail:8080"}, true, "ACCOUNT_ERASURE_SKYMAIL_URL"},
		"broken fallback":    {map[string]string{"ACCOUNT_ERASURE_SKYMAIL_URL": "skymail:8080"}, false, "ACCOUNT_ERASURE_SKYMAIL_URL"},
		"broken own address": {map[string]string{"COMMUNICATION_TRACKING_WITHDRAWAL_URL": "http://u:p@skymail"}, false, "COMMUNICATION_TRACKING_WITHDRAWAL_URL"},
		"no address":         {map[string]string{}, false, ""},
		"no client secret":   {map[string]string{"COMMUNICATION_TRACKING_WITHDRAWAL_URL": "http://skymail:8080", "KEYCLOAK_CLIENT_SECRET": " "}, false, "COMMUNICATION_TRACKING_WITHDRAWAL_URL"},
	} {
		env := map[string]string{}
		for k, v := range creds {
			env[k] = v
		}
		for k, v := range c.env {
			env[k] = v
		}
		w, from := newTrackingWithdrawals(func(k string) string { return env[k] }, "http://keycloak:8080", "e-skylab", commpref.NewMemoryStore())
		if w.Sending() != c.sending || from != c.fromWant {
			t.Fatalf("%s: sending=%v from=%q", name, w.Sending(), from)
		}
	}
	w, _ := newTrackingWithdrawals(func(k string) string {
		if k == "COMMUNICATION_TRACKING_WITHDRAWAL_URL" {
			return "http://skymail:8080"
		}
		return ""
	}, "", "", commpref.NewMemoryStore())
	if w.Sending() {
		t.Fatal("sending without Keycloak")
	}
}
