// Package erasure sends core's Erasure command to the services that hold a
// person's data (ADR-0051, docs/account-erasure-command.md).
//
// The command carries the person's addresses for the length of one call. This
// package never writes them, or the subject, to a log line, an error message,
// a metric or a store.
package erasure

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// DefaultAlertAfter is when an open request becomes overdue: day 20,
	// ten days ahead of the KVKK 30-day deadline.
	DefaultAlertAfter = 480 * time.Hour
	// DefaultPeriodicDestructionInterval is 90 days, valid whether or not the
	// data controller owes a retention and destruction policy (spec §6).
	DefaultPeriodicDestructionInterval = 90 * 24 * time.Hour
	// MaxPeriodicDestructionInterval is the regulation's longest period, six
	// months, rounded up to whole days.
	MaxPeriodicDestructionInterval = 184 * 24 * time.Hour
)

const enabledFlag = "ACCOUNT_ERASURE_WORKER_ENABLED"

// Service is one registry entry: the saga step, the variable holding the
// service's internal base URL and the token scope that carries its erase role.
type Service struct {
	Name   string
	Step   user.DeletionStep
	URLVar string
	Scope  string
}

var registry = []Service{
	{Name: "skymail", Step: user.DeletionStepEraseSkyMail, URLVar: "ACCOUNT_ERASURE_SKYMAIL_URL", Scope: "account-erase-skymail"},
	{Name: "cms", Step: user.DeletionStepEraseCMS, URLVar: "ACCOUNT_ERASURE_CMS_URL", Scope: "account-erase-cms"},
	{Name: "forms", Step: user.DeletionStepEraseForms, URLVar: "ACCOUNT_ERASURE_FORMS_URL", Scope: "account-erase-forms"},
}

// Registry returns every service that holds personal data outside core, in
// saga order. A new service that stores personal data is not finished until
// it has an entry here.
func Registry() []Service {
	return append([]Service(nil), registry...)
}

// Endpoint is a registry entry with its configured internal base URL.
type Endpoint struct {
	Service Service
	BaseURL string
}

// Config is the erasure command configuration.
type Config struct {
	Endpoints    []Endpoint
	ClientID     string
	ClientSecret string
	// AlertAfter is how long after creation an open request is overdue.
	AlertAfter time.Duration
	// PeriodicDestructionInterval is the one configured periodic-destruction
	// period (KVKK deletion regulation art. 11). Backup and log retention caps
	// and every cleanup job this work adds take it from here.
	PeriodicDestructionInterval time.Duration
}

// ConfigFromEnv reads the configuration. While the worker is off nothing is
// read or required. While it is on, every service URL and the client
// credentials are required, and an error names the variable, never its value.
func ConfigFromEnv(getenv func(string) string, enabled bool) (Config, error) {
	config := Config{
		AlertAfter:                  DefaultAlertAfter,
		PeriodicDestructionInterval: DefaultPeriodicDestructionInterval,
	}
	if !enabled {
		return config, nil
	}
	for _, service := range registry {
		raw, err := required(getenv, service.URLVar)
		if err != nil {
			return Config{}, err
		}
		base, err := baseURL(service.URLVar, raw)
		if err != nil {
			return Config{}, err
		}
		config.Endpoints = append(config.Endpoints, Endpoint{Service: service, BaseURL: base})
	}
	var err error
	if config.ClientID, err = required(getenv, "ACCOUNT_ERASURE_CLIENT_ID"); err != nil {
		return Config{}, err
	}
	if config.ClientSecret, err = required(getenv, "ACCOUNT_ERASURE_CLIENT_SECRET"); err != nil {
		return Config{}, err
	}
	if config.AlertAfter, err = duration(getenv, "ACCOUNT_ERASURE_ALERT_AFTER", DefaultAlertAfter, 0); err != nil {
		return Config{}, err
	}
	if config.PeriodicDestructionInterval, err = duration(getenv, "PERIODIC_DESTRUCTION_INTERVAL", DefaultPeriodicDestructionInterval, MaxPeriodicDestructionInterval); err != nil {
		return Config{}, err
	}
	return config, nil
}

func required(getenv func(string) string, name string) (string, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required when %s=true", name, enabledFlag)
	}
	return value, nil
}

func baseURL(name, raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("%s must be an absolute http or https base URL without credentials, query or fragment", name)
	}
	return strings.TrimRight(raw, "/"), nil
}

func duration(getenv func(string) string, name string, fallback, max time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 || (max > 0 && value > max) {
		if max > 0 {
			return 0, fmt.Errorf("%s must be a positive duration of at most %s", name, max)
		}
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}
