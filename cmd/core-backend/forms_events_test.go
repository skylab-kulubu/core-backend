package main

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

func TestFormsEventsSwitch(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "false": false, "true": true, " true ": true} {
		got, err := formsEventsEnabled(func(string) string { return raw })
		if err != nil || got != want {
			t.Fatalf("%q: %t %v", raw, got, err)
		}
	}
	if _, err := formsEventsEnabled(func(string) string { return "on" }); err == nil {
		t.Fatal("on was taken")
	}
}

// The events' Ticket rule runs with the forms service's authority, as the
// HTTP report does: the real authorizer lets it report an answer.
func TestFormsEventPrincipalMayReportAnAnswer(t *testing.T) {
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	if !az.Allow(formsEventPrincipal, authz.Resource{Type: authz.TypeFormResponse}, authz.Create) {
		t.Fatal("the forms events' principal may not report an answer")
	}
}

func TestFormsClientNeedsTheAddressAndCoresAccount(t *testing.T) {
	env := map[string]string{"KEYCLOAK_CLIENT_ID": "core", "KEYCLOAK_CLIENT_SECRET": "s"}
	getenv := func(k string) string { return env[k] }
	if c, _, err := newFormsClient(getenv, "http://kc", "e-skylab"); c != nil || err != nil {
		t.Fatalf("no address: %v %v", c, err)
	}
	env[formsfeed.URLEnv] = "http://forms:8080"
	c, from, err := newFormsClient(getenv, "http://kc", "e-skylab")
	if err != nil || c == nil || from != formsfeed.URLEnv || c.BaseURL != "http://forms:8080" {
		t.Fatalf("%v %q %v", c, from, err)
	}
	delete(env, "KEYCLOAK_CLIENT_SECRET")
	if c, _, _ := newFormsClient(getenv, "http://kc", "e-skylab"); c != nil {
		t.Fatal("a client without core's account")
	}
}
