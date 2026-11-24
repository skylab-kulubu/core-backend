package main

import (
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
	} {
		got, err := consoleAuditFromEnv(env(tc.vars))
		if err != nil || got != tc.want {
			t.Fatalf("%s: %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
	for _, bad := range []map[string]string{
		{"KEYCLOAK_CONSOLE_AUDIT": "true"},
		{"KEYCLOAK_CONSOLE_AUDIT_INTERVAL": "5s"},
		{"KEYCLOAK_CONSOLE_AUDIT_INTERVAL": "1h"},
		{"KEYCLOAK_CONSOLE_AUDIT_INTERVAL": "soon"},
	} {
		if _, err := consoleAuditFromEnv(env(bad)); err == nil {
			t.Fatalf("%v must be refused", bad)
		}
	}
}
