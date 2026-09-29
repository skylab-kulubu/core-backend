package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// The service starts only with an allowlist it can read; the port and
// ffmpeg's path have defaults.
func TestConfigFromEnv(t *testing.T) {
	t.Parallel()
	got, err := configFromEnv(env(map[string]string{"MEDIA_FRAME_ALLOWED_HOSTS": "https://abc.r2.cloudflarestorage.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.addr != ":8080" || got.ffmpeg != "ffmpeg" || !slices.Equal(got.allowed, []string{"abc.r2.cloudflarestorage.com"}) {
		t.Fatalf("config %+v", got)
	}
	got, err = configFromEnv(env(map[string]string{"MEDIA_FRAME_ALLOWED_HOSTS": "a.test", "PORT": "9000", "MEDIA_FRAME_FFMPEG": "/usr/bin/ffmpeg"}))
	if err != nil || got.addr != ":9000" || got.ffmpeg != "/usr/bin/ffmpeg" {
		t.Fatalf("config %+v, %v", got, err)
	}
	for name, values := range map[string]map[string]string{
		"no allowlist":      {},
		"an IP allowed":     {"MEDIA_FRAME_ALLOWED_HOSTS": "10.0.0.1"},
		"a port not a port": {"MEDIA_FRAME_ALLOWED_HOSTS": "a.test", "PORT": "http"},
		"a port too high":   {"MEDIA_FRAME_ALLOWED_HOSTS": "a.test", "PORT": "70000"},
	} {
		if _, err := configFromEnv(env(values)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// `media-frame health` is the container's health check: 0 when the service
// on its port answers /health, 1 otherwise.
func TestHealthCommand(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	var errOut bytes.Buffer
	if code := health(env(map[string]string{"PORT": port}), &errOut); code != 0 {
		t.Fatalf("a service that answers: exit %d %s", code, errOut.String())
	}
	server.Close()
	if code := health(env(map[string]string{"PORT": port}), &errOut); code != 1 {
		t.Fatalf("a service that is down: exit %d", code)
	}
}
