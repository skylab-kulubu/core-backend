package githubactivity

import (
	"errors"
	"strings"
	"testing"
)

// A panic in a repository's worker is that worker's error, not the end of the
// process.
func TestProtectTurnsAPanicIntoAnError(t *testing.T) {
	t.Parallel()
	err := protect(func() error { panic("boom") })()
	if err == nil || !strings.Contains(err.Error(), "panicked: boom") {
		t.Fatalf("err %v", err)
	}
	want := errors.New("plain")
	if got := protect(func() error { return want })(); !errors.Is(got, want) {
		t.Fatalf("err %v", got)
	}
}

func TestRedactNamesRepositoriesByHash(t *testing.T) {
	t.Parallel()
	got := redact("/repos/skylab-kulubu/secret-infra/commits")
	if strings.Contains(got, "secret-infra") || got != "/repos/skylab-kulubu/repo-"+repoHash("skylab-kulubu", "secret-infra")+"/commits" {
		t.Fatalf("%q", got)
	}
	if len(repoHash("a", "b")) != 8 || repoHash("Org", "Repo") != repoHash("org", "repo") {
		t.Fatal("hash")
	}
	for _, path := range []string{"/graphql", "/app/installations/1/access_tokens", "/orgs/skylab-kulubu/installation"} {
		if redact(path) != path {
			t.Fatalf("%q changed", path)
		}
	}
}
