package main

import (
	"reflect"
	"testing"
	"time"
)

func TestConsoleAuditFromEnv(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	for name, tc := range map[string]struct {
		vars map[string]string
		want consoleAuditConfig
	}{
		"unset":    {map[string]string{}, consoleAuditConfig{Interval: time.Minute}},
		"on":       {map[string]string{"KEYCLOAK_CONSOLE_AUDIT": " on "}, consoleAuditConfig{On: true, Interval: time.Minute}},
		"interval": {map[string]string{"KEYCLOAK_CONSOLE_AUDIT": "on", "KEYCLOAK_CONSOLE_AUDIT_INTERVAL": "30s"}, consoleAuditConfig{On: true, Interval: 30 * time.Second}},
		"off":      {map[string]string{"KEYCLOAK_CONSOLE_AUDIT": "off"}, consoleAuditConfig{Interval: time.Minute}},
		"automation": {map[string]string{"KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS": " account-center-config, 5B0E3C1A-1111-4222-8333-944455556666 ,,"},
			consoleAuditConfig{Interval: time.Minute, AutomationClients: []string{"account-center-config", "5b0e3c1a-1111-4222-8333-944455556666"}}},
	} {
		got, err := consoleAuditFromEnv(env(tc.vars))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
	for _, bad := range []map[string]string{
		{"KEYCLOAK_CONSOLE_AUDIT": "true"},
		{"KEYCLOAK_CONSOLE_AUDIT_INTERVAL": "5s"},
		{"KEYCLOAK_CONSOLE_AUDIT_INTERVAL": "1h"},
		{"KEYCLOAK_CONSOLE_AUDIT_INTERVAL": "soon"},
		{"KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS": "admin-cli"},
		{"KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS": "account-center-config,security-admin-console"},
		{"KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS": "bad client"},
	} {
		if _, err := consoleAuditFromEnv(env(bad)); err == nil {
			t.Fatalf("%v must be refused", bad)
		}
	}
}
