// Package testrabbitmq provides a disposable real RabbitMQ for integration
// tests.
package testrabbitmq

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// DefaultImage is RabbitMQ 4.2, the image (and digest) e-skylab-keycloak's
// integration tests run, so both sides test against the same broker.
const DefaultImage = "rabbitmq:4.2-management-alpine@sha256:643139a7e9b4d7e2c1d6a06295d0296c3c58e5de7666a09630b4564a15c951cd"

// ImageEnv names the variable that replaces DefaultImage.
const ImageEnv = "CORE_TEST_RABBITMQ_IMAGE"

// Vhost is the vhost the broker starts with, the one ops sets up.
const Vhost = "skylab"

// Broker is a running broker. Its administrator may do anything on Vhost;
// tests set up the topology with it, as ops does.
type Broker struct {
	Container string
	Host      string
	Port      int
	admin     string
	password  string
}

// Start launches the broker and registers its removal with t. Tests are
// skipped when Docker is unavailable.
func Start(t testing.TB) *Broker {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	image := DefaultImage
	if v := os.Getenv(ImageEnv); v != "" {
		image = v
	}
	b := &Broker{
		Container: fmt.Sprintf("core-rabbitmq-test-%d", time.Now().UnixNano()),
		admin:     "test-admin",
		password:  "test-admin-password",
	}
	// The image declares /var/lib/rabbitmq a VOLUME; a tmpfs there keeps
	// the test from leaving an anonymous volume behind and the queues'
	// logs in memory.
	run := exec.Command("docker", "run", "-d", "--rm", "--name", b.Container,
		"--memory", "512m",
		"--tmpfs", "/var/lib/rabbitmq",
		"-e", "RABBITMQ_DEFAULT_USER="+b.admin,
		"-e", "RABBITMQ_DEFAULT_PASS="+b.password,
		"-e", "RABBITMQ_DEFAULT_VHOST="+Vhost,
		"-p", "127.0.0.1::5672",
		image)
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("docker run rabbitmq: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", b.Container).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for b.Port == 0 {
		if ctx.Err() != nil {
			t.Fatal("rabbitmq port was not published before timeout")
		}
		out, err := exec.Command("docker", "port", b.Container, "5672/tcp").CombinedOutput()
		if err == nil {
			b.Host, b.Port = published(out)
		}
		if b.Port == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	var lastErr error
	for ctx.Err() == nil {
		conn, err := b.dial(b.admin, b.password)
		if err == nil {
			_ = conn.Close()
			// The administrator answers once the node has booted; the
			// CLI below waits for the same.
			b.Ctl(t, "await_startup")
			return b
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", "--tail", "50", b.Container).CombinedOutput()
	t.Fatalf("rabbitmq never became ready: %v\n%s", lastErr, logs)
	return nil
}

func published(out []byte) (string, int) {
	for _, line := range strings.Split(string(out), "\n") {
		address := strings.TrimSpace(line)
		if address == "" {
			continue
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			continue
		}
		return host, n
	}
	return "", 0
}

func (b *Broker) dial(user, password string) (*amqp.Connection, error) {
	return amqp.DialConfig("amqp://"+net.JoinHostPort(b.Host, strconv.Itoa(b.Port))+"/", amqp.Config{
		SASL:  []amqp.Authentication{&amqp.PlainAuth{Username: user, Password: password}},
		Vhost: Vhost,
		Dial:  amqp.DefaultDial(5 * time.Second),
	})
}

// Admin opens a channel as the administrator; Cleanup closes it.
func (b *Broker) Admin(t testing.TB) *amqp.Channel {
	t.Helper()
	conn, err := b.dial(b.admin, b.password)
	if err != nil {
		t.Fatalf("rabbitmq admin connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("rabbitmq admin channel: %v", err)
	}
	return ch
}

// Ctl runs rabbitmqctl in the container and returns its output.
func (b *Broker) Ctl(t testing.TB, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"exec", b.Container, "rabbitmqctl", "--quiet"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("rabbitmqctl %s: %v %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// AddUser creates a user with the given permissions on Vhost (regular
// expressions, as rabbitmqctl set_permissions takes them).
func (b *Broker) AddUser(t testing.TB, user, password, configure, write, read string) {
	t.Helper()
	b.Ctl(t, "add_user", user, password)
	b.Ctl(t, "set_permissions", "-p", Vhost, user, configure, write, read)
}
