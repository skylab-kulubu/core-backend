// Package testredis provides a disposable real Redis fixture for integration tests.
package testredis

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func Start(t testing.TB) *redis.Client {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("core-access-redis-test-%d", time.Now().UnixNano())
	// The image declares /data as a VOLUME; tmpfs keeps each test from leaving
	// an anonymous volume behind (Cleanup's `rm -f` bypasses `--rm`).
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"--tmpfs", "/data",
		"-p", "127.0.0.1::6379", "redis:7-alpine", "redis-server",
		"--appendonly", "yes", "--appendfsync", "always", "--maxmemory-policy", "noeviction")
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("docker run redis: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var address string
	for ctx.Err() == nil {
		out, err := exec.Command("docker", "port", name, "6379/tcp").CombinedOutput()
		if err == nil {
			address = publishedAddress(out)
			if address != "" {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if address == "" {
		t.Fatal("redis port was not published before timeout")
	}

	client := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() { _ = client.Close() })
	for ctx.Err() == nil {
		if err := client.Ping(ctx).Err(); err == nil {
			return client
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("redis never became ready")
	return nil
}

func publishedAddress(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		address := strings.TrimSpace(line)
		if address == "" {
			continue
		}
		if i := strings.LastIndex(address, "://"); i >= 0 {
			address = address[i+3:]
		}
		return address
	}
	return ""
}
