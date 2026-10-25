// Package events is core's consumer of Integration events (ADR-0071,
// docs/integration-events.md): it reads the queues ops sets up on RabbitMQ,
// hands each event to the handler registered for its queue, and records the
// event's source and id in the same database transaction as the handler's
// writes, so a repeat delivery has no second effect. It never declares an
// exchange or a queue: core's RabbitMQ user may only read its own queues.
package events

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	// EnabledEnv turns the consumer on with true. Off (the default), core
	// never connects to RabbitMQ.
	EnabledEnv = "EVENTS_ENABLED"
	// HostEnv, PortEnv, VhostEnv, UsernameEnv and PasswordEnv are the
	// broker and core's user on it; in a deployment they are OpenBao
	// references (ADR-0049). The user name is a secret's too: it alternates
	// between core_a and core_b when the password rotates.
	HostEnv     = "RABBITMQ_HOST"
	PortEnv     = "RABBITMQ_PORT"
	VhostEnv    = "RABBITMQ_VHOST"
	UsernameEnv = "RABBITMQ_USERNAME"
	PasswordEnv = "RABBITMQ_PASSWORD"
)

const (
	// DefaultPort is AMQP's, without TLS (ADR-0071: no TLS on the
	// environment's internal network in the first version).
	DefaultPort = 5672
	// DefaultVhost is the one vhost ops sets up.
	DefaultVhost = "skylab"
)

// Config is how core reaches the broker. Password is never printed.
type Config struct {
	Enabled  bool
	Host     string
	Port     int
	Vhost    string
	Username string
	Password string
}

// ConfigFromEnv reads the variables. Off, nothing else is read. On, the host,
// the user name and the password are required. A value core cannot read is an
// error, which names the variable and never the value.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	config := Config{Port: DefaultPort, Vhost: DefaultVhost}
	switch raw := strings.TrimSpace(getenv(EnabledEnv)); raw {
	case "", "false":
		return config, nil
	case "true":
		config.Enabled = true
	default:
		return Config{}, fmt.Errorf("%s must be true or false", EnabledEnv)
	}
	config.Host = strings.TrimSpace(getenv(HostEnv))
	if config.Host == "" || strings.ContainsAny(config.Host, "/@: \t") {
		return Config{}, fmt.Errorf("%s must be a host name when %s=true", HostEnv, EnabledEnv)
	}
	if raw := strings.TrimSpace(getenv(PortEnv)); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("%s must be a port number from 1 to 65535", PortEnv)
		}
		config.Port = port
	}
	if raw := strings.TrimSpace(getenv(VhostEnv)); raw != "" {
		config.Vhost = raw
	}
	config.Username = strings.TrimSpace(getenv(UsernameEnv))
	if config.Username == "" {
		return Config{}, fmt.Errorf("%s is required when %s=true", UsernameEnv, EnabledEnv)
	}
	// The password is taken as it is: OpenBao hands it over without
	// surrounding space, and trimming could change a valid one.
	config.Password = getenv(PasswordEnv)
	if config.Password == "" {
		return Config{}, fmt.Errorf("%s is required when %s=true", PasswordEnv, EnabledEnv)
	}
	return config, nil
}

// addr is the broker's host and port.
func (c Config) addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// String is the startup line's summary: the broker, the vhost and the user,
// never the password.
func (c Config) String() string {
	if !c.Enabled {
		return fmt.Sprintf("off (%s=false); core does not connect to RabbitMQ", EnabledEnv)
	}
	return fmt.Sprintf("on, %s vhost %q as %q", c.addr(), c.Vhost, c.Username)
}

// GoString keeps %#v from printing the password.
func (c Config) GoString() string { return "events.Config{" + c.String() + "}" }
