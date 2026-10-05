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

const (
	enabledFlag     = "ACCOUNT_ERASURE_WORKER_ENABLED"
	clientSecretVar = "ACCOUNT_ERASURE_CLIENT_SECRET"
)

// UngatedServiceWait is how long the erase step of a service without the
// account access gate waits after the identity is closed. Such a service still
// accepts an access token issued before Keycloak disabled the person until the
// token expires: Keycloak's default access token lifespan of 300 seconds plus
// the service's 30-second clock skew, rounded up (spec §3.2). Erasing after
// that also erases an edit made with such a token.
const UngatedServiceWait = 6 * time.Minute

// Service is one registry entry: the saga step, the variable holding the
// service's internal base URL and the token scope that carries its erase role.
type Service struct {
	Name   string
	Step   user.DeletionStep
	URLVar string
	Scope  string
	// WaitAfterIdentityClosed is how long the step waits after the person was
	// blocked, disabled and logged out before it sends the command. Zero for a
	// service that refuses a blocked person's token itself (the access gate).
	WaitAfterIdentityClosed time.Duration
}

var registry = []Service{
	{Name: "skymail", Step: user.DeletionStepEraseSkyMail, URLVar: "ACCOUNT_ERASURE_SKYMAIL_URL", Scope: "account-erase-skymail"},
	// The CMS is inscribed (ADR-0056), which keeps no accounts and has no
	// access gate; it holds the person's sub in its editor columns only.
	{Name: "cms", Step: user.DeletionStepEraseCMS, URLVar: "ACCOUNT_ERASURE_CMS_URL", Scope: "account-erase-cms", WaitAfterIdentityClosed: UngatedServiceWait},
	{Name: "forms", Step: user.DeletionStepEraseForms, URLVar: "ACCOUNT_ERASURE_FORMS_URL", Scope: "account-erase-forms"},
}

// Registry returns every service that holds personal data outside core, in
// saga order. A new service that stores personal data is not finished until
// it has an entry here.
func Registry() []Service {
	return append([]Service(nil), registry...)
}

// ServiceNamed returns the registry entry with that name: skymail, cms or
// forms.
func ServiceNamed(name string) (Service, bool) {
	for _, service := range registry {
		if service.Name == name {
			return service, true
		}
	}
	return Service{}, false
}

// Endpoint is a registry entry with its configured internal base URL.
type Endpoint struct {
	Service Service
	BaseURL string
}

// Config is the erasure command configuration.
type Config struct {
	Endpoints []Endpoint
	ClientID  string
	// ClientSecret reads ACCOUNT_ERASURE_CLIENT_SECRET from the configuration
	// each time it is called. Config keeps no copy of the secret, which
	// rotates nightly (ADR-0050). Nil while the worker is off.
	ClientSecret SecretSource
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
	// Startup only proves the secret is there; the value is not kept.
	secret := func() (string, error) { return required(getenv, clientSecretVar) }
	if _, err = secret(); err != nil {
		return Config{}, err
	}
	config.ClientSecret = secret
	if config.AlertAfter, err = duration(getenv, "ACCOUNT_ERASURE_ALERT_AFTER", DefaultAlertAfter, 0); err != nil {
		return Config{}, err
	}
	if config.PeriodicDestructionInterval, err = PeriodicDestructionIntervalFromEnv(getenv); err != nil {
		return Config{}, err
	}
	return config, nil
}

// PeriodicDestructionIntervalEnv holds the periodic destruction interval.
const PeriodicDestructionIntervalEnv = "PERIODIC_DESTRUCTION_INTERVAL"

// PeriodicDestructionIntervalFromEnv reads the periodic destruction interval
// on its own, whether or not the worker is on: the retention sweep's periods
// (docs/retention-sweep.md) are this interval too. Unset, it is 90 days; an
// error names the variable, never its value.
func PeriodicDestructionIntervalFromEnv(getenv func(string) string) (time.Duration, error) {
	return duration(getenv, PeriodicDestructionIntervalEnv, DefaultPeriodicDestructionInterval, MaxPeriodicDestructionInterval)
}

// ClientFromEnv builds the Erasure command client of one service for a
// command run beside the server, the replay after a restore (ADR-0053). It
// reads the service's URL variable, ACCOUNT_ERASURE_CLIENT_ID and
// ACCOUNT_ERASURE_CLIENT_SECRET as the worker does, whether or not the worker
// is on, and keeps no copy of the secret: the client reads it again for every
// token request. Every error names a variable, never its value.
func ClientFromEnv(getenv func(string) string, service Service, tokenURL string) (*Client, error) {
	raw, err := set(getenv, service.URLVar)
	if err != nil {
		return nil, err
	}
	base, err := baseURL(service.URLVar, raw)
	if err != nil {
		return nil, err
	}
	clientID, err := set(getenv, "ACCOUNT_ERASURE_CLIENT_ID")
	if err != nil {
		return nil, err
	}
	secret := func() (string, error) { return set(getenv, clientSecretVar) }
	if _, err := secret(); err != nil {
		return nil, err
	}
	return NewClient(Endpoint{Service: service, BaseURL: base}, tokenURL, clientID, secret), nil
}

func set(getenv func(string) string, name string) (string, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
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
