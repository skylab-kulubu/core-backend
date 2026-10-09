package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `core-backend healthcheck` is the container's health check: 0 when this
// container's core answers /v1/ready with 204, 1 otherwise (Docker reserves
// 2), saying why on stderr.
func TestHealthcheckCommand(t *testing.T) {
	t.Parallel()
	status := http.StatusNoContent
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.WriteHeader(status)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))

	var errOut bytes.Buffer
	if code := runHealthcheck(nil, env(map[string]string{"PORT": port}), &errOut); code != 0 {
		t.Fatalf("ready: exit %d %s", code, errOut.String())
	}
	if asked != "/v1/ready" {
		t.Fatalf("asked %q, want /v1/ready", asked)
	}
	status = http.StatusServiceUnavailable
	errOut.Reset()
	if code := runHealthcheck(nil, env(map[string]string{"PORT": port}), &errOut); code != 1 || !strings.Contains(errOut.String(), "503") {
		t.Fatalf("not ready: exit %d %q", code, errOut.String())
	}

	// Nothing listening.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closed, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	errOut.Reset()
	if code := runHealthcheck(nil, env(map[string]string{"PORT": closed}), &errOut); code != 1 || errOut.Len() == 0 {
		t.Fatalf("nothing listening: exit %d %q", code, errOut.String())
	}
	if code := runHealthcheck([]string{"extra"}, env(nil), &errOut); code != 1 {
		t.Fatalf("an argument: exit %d", code)
	}
}
