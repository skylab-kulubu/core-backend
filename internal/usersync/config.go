// Package usersync keeps core's User rows in step with Keycloak
// (docs/user-sync.md): every account Keycloak has gets its row and its Sky
// number, blanks in the row are filled from Keycloak, and the Welcome mail
// goes out once to every account created since WELCOME_MAIL_SINCE.
package usersync

import (
	"fmt"
	"strings"
	"time"
)

const (
	// EnabledEnv turns the sync off with false. It is on by default
	// wherever core reads Keycloak.
	EnabledEnv = "USER_SYNC_ENABLED"
	// IntervalEnv is the time between two passes (a Go duration).
	IntervalEnv = "USER_SYNC_INTERVAL"
	// WelcomeSinceEnv is the cutoff of the Welcome mail (RFC 3339): an
	// account Keycloak created before it gets none.
	WelcomeSinceEnv = "WELCOME_MAIL_SINCE"
)

const (
	// DefaultInterval is five minutes.
	DefaultInterval = 5 * time.Minute
	minInterval     = time.Minute
	maxInterval     = 24 * time.Hour
)

// DefaultWelcomeSince is when sign-up by e-mail went live: 2026-10-19 00:00
// Istanbul time. Accounts created since then and never welcomed get the mail.
var DefaultWelcomeSince = time.Date(2026, 10, 18, 21, 0, 0, 0, time.UTC)

// Config is how the sync runs.
type Config struct {
	Enabled      bool
	Interval     time.Duration
	WelcomeSince time.Time
}

// ConfigFromEnv reads the three variables. A value core cannot read is an
// error: a mistyped cutoff would send or withhold mails silently.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	config := Config{Enabled: true, Interval: DefaultInterval, WelcomeSince: DefaultWelcomeSince}
	switch raw := strings.TrimSpace(getenv(EnabledEnv)); raw {
	case "", "true":
	case "false":
		config.Enabled = false
	default:
		return Config{}, fmt.Errorf("%s must be true or false, not %q", EnabledEnv, raw)
	}
	if raw := strings.TrimSpace(getenv(IntervalEnv)); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval < minInterval || interval > maxInterval {
			return Config{}, fmt.Errorf("%s must be a duration from %s to %s, not %q", IntervalEnv, minInterval, maxInterval, raw)
		}
		config.Interval = interval
	}
	if raw := strings.TrimSpace(getenv(WelcomeSinceEnv)); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be an RFC 3339 time (2026-10-19T00:00:00+03:00), not %q", WelcomeSinceEnv, raw)
		}
		config.WelcomeSince = since.UTC()
	}
	return config, nil
}

// String is the startup line's summary.
func (c Config) String() string {
	if !c.Enabled {
		return fmt.Sprintf("off (%s=false)", EnabledEnv)
	}
	return fmt.Sprintf("every %s, Welcome mail for accounts created since %s", c.Interval, c.WelcomeSince.Format(time.RFC3339))
}
