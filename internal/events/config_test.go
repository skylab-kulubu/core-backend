package events

import (
	"fmt"
	"strings"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestConfigOffByDefault(t *testing.T) {
	for _, raw := range []string{"", "false", " false "} {
		config, err := ConfigFromEnv(env(map[string]string{EnabledEnv: raw, HostEnv: "ignored"}))
		if err != nil || config.Enabled {
			t.Fatalf("%q: %+v %v", raw, config.Enabled, err)
		}
		// Off reads nothing else: no broker, no user.
		if config.Host != "" || config.Username != "" || config.Password != "" {
			t.Fatalf("%q: read the broker settings while off", raw)
		}
	}
}

func TestConfigOn(t *testing.T) {
	config, err := ConfigFromEnv(env(map[string]string{
		EnabledEnv: "true", HostEnv: "rabbitmq", UsernameEnv: "core_a", PasswordEnv: " s3cret pass ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.Host != "rabbitmq" || config.Port != DefaultPort || config.Vhost != DefaultVhost ||
		config.Username != "core_a" || config.Password != " s3cret pass " {
		t.Fatalf("config = %#v (password kept as given: %v)", config, config.Password == " s3cret pass ")
	}
	config, err = ConfigFromEnv(env(map[string]string{
		EnabledEnv: "true", HostEnv: "events.internal", PortEnv: "5673", VhostEnv: "other",
		UsernameEnv: "core_b", PasswordEnv: "p",
	}))
	if err != nil || config.Port != 5673 || config.Vhost != "other" || config.addr() != "events.internal:5673" {
		t.Fatalf("config = %#v, %v", config, err)
	}
}

func TestConfigRefusesWhatItCannotRead(t *testing.T) {
	base := map[string]string{EnabledEnv: "true", HostEnv: "rabbitmq", UsernameEnv: "core_a", PasswordEnv: "the-password"}
	for name, change := range map[string]map[string]string{
		"flag":          {EnabledEnv: "yes"},
		"no host":       {HostEnv: ""},
		"host with url": {HostEnv: "amqp://core_a:x@rabbitmq"},
		"host:port":     {HostEnv: "rabbitmq:5672"},
		"port":          {PortEnv: "0"},
		"port text":     {PortEnv: "amqp"},
		"no user":       {UsernameEnv: " "},
		"no password":   {PasswordEnv: ""},
	} {
		values := map[string]string{}
		for k, v := range base {
			values[k] = v
		}
		for k, v := range change {
			values[k] = v
		}
		_, err := ConfigFromEnv(env(values))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), "the-password") || strings.Contains(err.Error(), "amqp://") {
			t.Fatalf("%s: the error carries a value: %v", name, err)
		}
	}
}

// The password appears in no summary, whatever verb prints the config.
func TestConfigNeverPrintsThePassword(t *testing.T) {
	config := Config{Enabled: true, Host: "rabbitmq", Port: 5672, Vhost: "skylab", Username: "core_a", Password: "hunter2-secret"}
	for _, text := range []string{config.String(), fmt.Sprintf("%v", config), fmt.Sprintf("%+v", config), fmt.Sprintf("%#v", config), fmt.Sprint(config)} {
		if strings.Contains(text, "hunter2-secret") {
			t.Fatalf("printed the password: %s", text)
		}
	}
	if got := config.String(); got != `on, rabbitmq:5672 vhost "skylab" as "core_a"` {
		t.Fatalf("String = %s", got)
	}
	if got := (Config{}).String(); !strings.Contains(got, "off") {
		t.Fatalf("off String = %s", got)
	}
}
