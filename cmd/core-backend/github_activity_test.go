package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/githubactivity"
)

func TestGithubActivityFromEnv(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	stored := base64.StdEncoding.EncodeToString(pemKey)
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	run := func(values map[string]string) (any, string) {
		var lines []string
		source := githubActivityFromEnv(func(name string) string { return values[name] }, az,
			func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
		return source, strings.Join(lines, "\n")
	}

	source, line := run(map[string]string{})
	if source != nil || !strings.HasPrefix(line, "github activity: off") {
		t.Fatalf("unset: %v %q", source, line)
	}

	source, line = run(map[string]string{githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "4242", githubactivity.PrivateKeyEnv: stored})
	if _, ok := source.(*githubactivity.Service); !ok || line != "github activity: on (org skylab-kulubu, app 4242, installation found from the organisation, window 30 days; 4 workers, 10 pages a list, 45s a read)" {
		t.Fatalf("set: %T %q", source, line)
	}

	source, line = run(map[string]string{githubactivity.OrgEnv: "skylab-kulubu", githubactivity.AppIDEnv: "4242", githubactivity.PrivateKeyEnv: stored[:40]})
	if _, ok := source.(*githubactivity.Service); !ok || !strings.HasPrefix(line, "github activity: settings are wrong") ||
		!strings.Contains(line, githubactivity.PrivateKeyEnv) || strings.Contains(line, stored[:40]) {
		t.Fatalf("wrong: %T %q", source, line)
	}
}
