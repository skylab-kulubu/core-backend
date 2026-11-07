// Package apppref keeps App preferences (docs/app-preferences.md, ADR 0081):
// a person's small per-app settings, such as whether Forms' "Formlarım" list
// shows details. Core stores, limits and deletes each value; it never reads
// what is inside. An app reaches only its own namespace, picked by the
// token's azp through APP_PREFERENCE_CLIENTS. Account center reads each
// app's key count and last change and resets an app, never the values.
package apppref

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The limits (ADR 0081 karar 5).
const (
	// MaxValueBytes is a value's largest compact JSON size.
	MaxValueBytes = 4096
	// MaxKeysPerApp is the most keys a person has in one app.
	MaxKeysPerApp = 32
	// MaxBytesPerApp is the most value bytes a person has in one app.
	MaxBytesPerApp = 32 << 10
	// MaxBodyBytes is a write's largest request body.
	MaxBodyBytes = 8 << 10
	// MaxSchemaVersion is the largest schemaVersion an app may give.
	MaxSchemaVersion = 1000
	// WritesPerMinute is each person's write budget across the routes.
	WritesPerMinute = 60
)

// ClientsEnv configures the client map: app:client pairs, separated by
// commas. Unset, empty or "none" turns App preferences off: every route
// answers 404.
const ClientsEnv = "APP_PREFERENCE_CLIENTS"

// AccountApp is Account center's namespace. A client mapped to it also reads
// every app's summary and resets any app.
const AccountApp = "account"

var (
	appName  = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	keyName  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	clientID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
)

var (
	// ErrOff is App preferences turned off (no client map).
	ErrOff = errors.New("app preferences: off")
	// ErrUnknownApp is an app the client map does not name.
	ErrUnknownApp = errors.New("app preferences: unknown app")
	// ErrPersonOnly is a service account's token: preferences are a person's.
	ErrPersonOnly = errors.New("app preferences: a person's token only")
	// ErrWrongClient is a token whose client is not the app's.
	ErrWrongClient = errors.New("app preferences: the token's client is not this app's")
	// ErrInvalidKey is a key outside the allowed names.
	ErrInvalidKey = errors.New("app preferences: invalid key")
	// ErrInvalidValue is a value that is not JSON, is null, or comes with a
	// schemaVersion out of range.
	ErrInvalidValue = errors.New("app preferences: invalid value")
	// ErrValueTooLarge is a value over MaxValueBytes.
	ErrValueTooLarge = errors.New("app preferences: value too large")
	// ErrQuota is a write that takes the app over MaxKeysPerApp or
	// MaxBytesPerApp.
	ErrQuota = errors.New("app preferences: app quota exceeded")
	// ErrConflict is an If-Match that does not hold the stored version.
	ErrConflict = errors.New("app preferences: version conflict")
	// ErrSubjectInactive is a write for a person being erased or erased.
	ErrSubjectInactive = errors.New("app preferences: account subject is not active")
)

// Config is the client map: each Keycloak client's app.
type Config struct {
	clients map[string]string
}

// ConfigFromEnv reads ClientsEnv.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	return ParseClients(getenv(ClientsEnv))
}

// ParseClients reads a ClientsEnv value. One app may have several clients
// (sandbox, local); a client has one app.
func ParseClients(value string) (Config, error) {
	value = strings.TrimSpace(value)
	config := Config{clients: map[string]string{}}
	if value == "" || value == "none" {
		return config, nil
	}
	for _, entry := range strings.Split(value, ",") {
		app, client, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || !appName.MatchString(app) || !clientID.MatchString(client) {
			return Config{}, fmt.Errorf("%s: %q is not app:client", ClientsEnv, entry)
		}
		if _, taken := config.clients[client]; taken {
			return Config{}, fmt.Errorf("%s: client %s named twice", ClientsEnv, client)
		}
		config.clients[client] = app
	}
	return config, nil
}

// Enabled is a non-empty client map.
func (c Config) Enabled() bool { return len(c.clients) > 0 }

// AppOf is client's app, or "".
func (c Config) AppOf(client string) string { return c.clients[client] }

// Known says whether app has a client.
func (c Config) Known(app string) bool {
	for _, a := range c.clients {
		if a == app {
			return true
		}
	}
	return false
}

// Apps are the configured apps, sorted.
func (c Config) Apps() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range c.clients {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// String is the startup line's account of the map.
func (c Config) String() string {
	if !c.Enabled() {
		return "off (" + ClientsEnv + " is not set); the routes answer 404"
	}
	pairs := make([]string, 0, len(c.clients))
	for client, app := range c.clients {
		pairs = append(pairs, app+":"+client)
	}
	sort.Strings(pairs)
	return "on for " + strings.Join(pairs, ",")
}

// Caller is who asks: the person, and the client their token was issued to.
type Caller struct {
	Person         uuid.UUID
	Client         string
	ServiceAccount bool
}

// Item is one stored preference.
type Item struct {
	Key           string
	Value         json.RawMessage
	SchemaVersion int
	Version       int64
	UpdatedAt     time.Time
}

// Summary is one app's preferences without their values, for Account center.
type Summary struct {
	App       string
	Keys      int
	UpdatedAt time.Time
}

// Match is a write's If-Match: Any for "*" (the key must exist), else the
// version it must hold.
type Match struct {
	Any     bool
	Version int64
}

// Write is one validated value to store.
type Write struct {
	// Value is the compact JSON.
	Value         json.RawMessage
	SchemaVersion int
	// Match is nil without an If-Match.
	Match *Match
}

// ValidKey says whether key is an allowed key name.
func ValidKey(key string) bool { return keyName.MatchString(key) }
