package httpx_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/githubactivity"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// quietGitHub is an organisation with one public repository nobody pushed to
// lately: an installation token and one GraphQL page.
func quietGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/9/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_quiet","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
		case "/graphql":
			_, _ = io.WriteString(w, `{"data":{"organization":{"repositories":{"pageInfo":{"hasNextPage":false,"endCursor":"1"},"nodes":[`+
				`{"name":"old","url":"https://github.com/skylab-kulubu/old","isArchived":false,"isFork":false,"isPrivate":false,`+
				`"visibility":"PUBLIC","pushedAt":"2020-01-01T00:00:00Z","defaultBranchRef":{"name":"main"},"pullRequests":{"totalCount":1}}]}}}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func githubActivityApp(t *testing.T, source func(az authz.Authorizer) *githubactivity.Service) (*fiber.App, *testauth.Bundle) {
	t.Helper()
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	if source != nil {
		deps.GithubActivity = source(authz.NewAuthorizer(authz.DefaultPolicy()))
	}
	return httpx.New(deps), keys
}

func githubActivityRequest(t *testing.T, app *fiber.App, token string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodGet, "/v1/dashboard/github-activity", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestGithubActivityRoute(t *testing.T) {
	t.Parallel()
	gh := quietGitHub(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, keys := githubActivityApp(t, func(az authz.Authorizer) *githubactivity.Service {
		return githubactivity.New(githubactivity.Config{Org: "skylab-kulubu", AppID: 1, InstallationID: 9, PrivateKey: key, WindowDays: 30},
			az, githubactivity.Options{APIURL: gh.URL, HTTP: gh.Client()})
	})
	yk := keys.Token(t, jwt.MapClaims{"sub": "11111111-1111-1111-1111-111111111111", "email": "yk@example.com", "groups": []string{"/UYELER/YK"}})
	member := keys.Token(t, jwt.MapClaims{"sub": "22222222-2222-2222-2222-222222222222", "email": "m@example.com", "groups": []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}})

	if resp := githubActivityRequest(t, app, ""); resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
	if resp := githubActivityRequest(t, app, member); resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("team leader: %d", resp.StatusCode)
	}
	resp := githubActivityRequest(t, app, yk)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("YK: %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get(fiber.HeaderCacheControl); got != "private, max-age=60" {
		t.Fatalf("Cache-Control %q", got)
	}
	var activity githubactivity.Activity
	if err := json.Unmarshal(body, &activity); err != nil {
		t.Fatal(err)
	}
	if activity.Org != "skylab-kulubu" || activity.Window.Days != 30 || activity.Totals.OpenPullRequests != 1 ||
		activity.Repositories == nil || activity.Events == nil || activity.Stale {
		t.Fatalf("%s", body)
	}
	if !strings.Contains(string(body), `"repositories":[]`) || !strings.Contains(string(body), `"events":[]`) {
		t.Fatalf("empty lists must be [] for the dashboard: %s", body)
	}
}

func TestGithubActivityRouteUnsetIs404(t *testing.T) {
	t.Parallel()
	app, keys := githubActivityApp(t, nil)
	yk := keys.Token(t, jwt.MapClaims{"sub": "11111111-1111-1111-1111-111111111111", "email": "yk@example.com", "groups": []string{"/UYELER/YK"}})
	if resp := githubActivityRequest(t, app, yk); resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("unset: %d", resp.StatusCode)
	}
}

func TestGithubActivityRouteMisconfiguredIs503(t *testing.T) {
	t.Parallel()
	app, keys := githubActivityApp(t, func(az authz.Authorizer) *githubactivity.Service {
		return githubactivity.Unavailable(az, errors.New(githubactivity.PrivateKeyEnv+" is required"))
	})
	yk := keys.Token(t, jwt.MapClaims{"sub": "11111111-1111-1111-1111-111111111111", "email": "yk@example.com", "groups": []string{"/UYELER/YK"}})
	resp := githubActivityRequest(t, app, yk)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusServiceUnavailable || resp.Header.Get(fiber.HeaderRetryAfter) != "60" ||
		resp.Header.Get(fiber.HeaderCacheControl) != "no-store" || !strings.Contains(string(body), `"code":"github_activity_unavailable"`) {
		t.Fatalf("misconfigured: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	if strings.Contains(string(body), githubactivity.PrivateKeyEnv) {
		t.Fatalf("the answer names the setting: %s", body)
	}
}
