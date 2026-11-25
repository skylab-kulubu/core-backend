package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/consoleaudit"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// consoleAuditConfig is the console audit's settings (docs/console-audit.md).
type consoleAuditConfig struct {
	// On is KEYCLOAK_CONSOLE_AUDIT: unset or off is off, on is on.
	On bool
	// Interval is KEYCLOAK_CONSOLE_AUDIT_INTERVAL: unset is
	// consoleaudit.DefaultInterval.
	Interval time.Duration
	// AutomationClients is KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS: the
	// club realm's confidential clients whose service accounts reconcile
	// it (account-center-config), by client id or internal id, comma
	// separated. admin-cli is refused: people sign in through it.
	AutomationClients []string
}

// publicAdminClients are Keycloak's public admin clients: people sign in
// through them, so a change made through them is never silenced.
var publicAdminClients = []string{"admin-cli", "security-admin-console"}

// automationClientPattern is a Keycloak client id as the setting takes it.
var automationClientPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,255}$`)

// consoleAuditFromEnv reads the console audit's settings. A value that does
// not parse stops core.
func consoleAuditFromEnv(getenv func(string) string) (consoleAuditConfig, error) {
	on, err := onOff(getenv, "KEYCLOAK_CONSOLE_AUDIT")
	if err != nil {
		return consoleAuditConfig{}, err
	}
	cfg := consoleAuditConfig{On: on, Interval: consoleaudit.DefaultInterval}
	if raw := strings.TrimSpace(getenv("KEYCLOAK_CONSOLE_AUDIT_INTERVAL")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < consoleaudit.MinInterval || d > consoleaudit.MaxInterval {
			return consoleAuditConfig{}, fmt.Errorf("KEYCLOAK_CONSOLE_AUDIT_INTERVAL must be a duration from %s to %s, not %q",
				consoleaudit.MinInterval, consoleaudit.MaxInterval, raw)
		}
		cfg.Interval = d
	}
	for _, raw := range strings.Split(getenv("KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS"), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if id, err := uuid.Parse(raw); err == nil {
			cfg.AutomationClients = append(cfg.AutomationClients, id.String())
			continue
		}
		if !automationClientPattern.MatchString(raw) || slices.Contains(publicAdminClients, raw) {
			return consoleAuditConfig{}, fmt.Errorf("KEYCLOAK_CONSOLE_AUDIT_AUTOMATION_CLIENTS takes confidential reconciler clients (account-center-config), comma separated, not %q", raw)
		}
		cfg.AutomationClients = append(cfg.AutomationClients, raw)
	}
	return cfg, nil
}

// startConsoleAudit starts the reader of Keycloak's console changes until
// ctx is cancelled and answers its metrics and a channel that closes once
// it has stopped. Off, or without Keycloak, nothing is read: turned on
// later, it reads from that moment on.
//
// membership is the membership mail and the privilege notice, terms the
// management membership terms, roles the privilege notice of role mappings.
func startConsoleAudit(ctx context.Context, cfg consoleAuditConfig, dir identity.Directory, pool *pgxpool.Pool, audit teamaudit.ConsoleRecorder,
	membership, terms identity.MembershipNotifier, roles identity.GroupRolesNotifier, logf func(string, ...any)) (interface{ Prometheus() string }, <-chan struct{}) {
	off := make(chan struct{})
	close(off)
	if !cfg.On {
		logf("keycloak console audit: off (KEYCLOAK_CONSOLE_AUDIT is not on)")
		return consoleaudit.Off{}, off
	}
	source, ok := dir.(consoleaudit.Source)
	if !ok || pool == nil {
		logf("keycloak console audit: OFF (KEYCLOAK_CONSOLE_AUDIT is on but there is no Keycloak or no database)")
		return consoleaudit.Off{}, off
	}
	svc, err := consoleaudit.NewService(consoleaudit.Config{
		Source: source, Store: consoleaudit.NewPostgresStore(pool), Audit: audit,
		Membership: membership, Terms: terms, Roles: roles, AutomationClients: cfg.AutomationClients,
		Interval: cfg.Interval, Logf: logf,
	})
	if err != nil {
		logf("keycloak console audit: OFF, %v", err)
		return consoleaudit.Off{}, off
	}
	automation := "none: a reconciler's changes are mailed as the console's"
	if len(cfg.AutomationClients) > 0 {
		automation = fmt.Sprintf("%d client(s) recorded as keycloak:reconcile, no mail", len(cfg.AutomationClients))
	}
	logf("keycloak console audit: on (every %s; %s; automation %s)", cfg.Interval, strings.Join(consoleaudit.ResourceTypes, ","), automation)
	return svc, svc.Run(ctx)
}
