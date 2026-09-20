package httpx_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

type httpAccessGate struct {
	decision   accessgate.Decision
	readyErr   error
	readyCalls int
}

func (g *httpAccessGate) Check(context.Context, string) accessgate.Decision { return g.decision }
func (g *httpAccessGate) Ready(context.Context) error {
	g.readyCalls++
	return g.readyErr
}

func TestReadinessChecksTheGateWhileLivenessDoesNot(t *testing.T) {
	t.Parallel()

	gate := &httpAccessGate{decision: accessgate.Unavailable, readyErr: errors.New("redis unavailable")}
	app := memoryAppWithAccessGate(gate)
	for index, test := range []struct {
		path   string
		status int
	}{
		{path: "/v1/health", status: fiber.StatusNoContent},
		{path: "/v1/ready", status: fiber.StatusServiceUnavailable},
	} {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, test.path, nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != test.status {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("GET %s status=%d body=%s", test.path, response.StatusCode, body)
		}
		if gate.readyCalls != index {
			t.Fatalf("GET %s readiness calls=%d, want %d", test.path, gate.readyCalls, index)
		}
	}
}

func TestAccessGateMetricsExposeOnlyAggregateDecisionsAndReadinessFailures(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	metrics := accessgate.NewMetrics()
	gate := &httpAccessGate{decision: accessgate.Blocked, readyErr: errors.New("redis unavailable")}
	app := memoryAppWithAccessGateMetrics(gate, metrics, keys.Parse())
	subjectID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	request := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": subjectID.String(), "email": "metrics@example.test",
	}))
	response, err := app.Test(request)
	if err != nil || response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("blocked response=%v err=%v", response, err)
	}
	ready, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/ready", nil))
	if err != nil || ready.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("ready response=%v err=%v", ready, err)
	}
	scrape, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/metrics", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(scrape.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if scrape.StatusCode != fiber.StatusOK || !strings.Contains(scrape.Header.Get(fiber.HeaderContentType), "text/plain") {
		t.Fatalf("metrics status=%d content-type=%q body=%s", scrape.StatusCode, scrape.Header.Get(fiber.HeaderContentType), text)
	}
	if got := scrape.Header.Get(fiber.HeaderCacheControl); got != "no-store" {
		t.Fatalf("metrics cache-control=%q", got)
	}
	for _, line := range []string{
		"skylab_account_access_decisions_blocked_total 1\n",
		"skylab_account_access_readiness_failure_total 1\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("metrics missing %q:\n%s", line, text)
		}
	}
	digest := strings.TrimPrefix(accessgate.MarkerKey(subjectID.String()), accessgate.MarkerKeyPrefix)
	if strings.Contains(text, subjectID.String()) || strings.Contains(text, digest) || strings.Contains(text, accessgate.MarkerKey(subjectID.String())) {
		t.Fatalf("metrics exposed account material:\n%s", text)
	}
}

func TestAuthenticatedShortLinkHopCannotDowngradeBlockedOrUnavailableSubjectToAnonymous(t *testing.T) {
	t.Parallel()

	keys := testauth.New(t)
	gate := &httpAccessGate{decision: accessgate.Allowed}
	app := memoryAppWithAccessGate(gate, keys.Parse())
	ownerID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	token := keys.Token(t, jwt.MapClaims{
		"sub": ownerID.String(), "email": "owner@example.test", "groups": []string{"/UYELER/YK"},
	})
	create := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://example.test","alias":"gate-hop"}`))
	create.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	create.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	if response, err := app.Test(create); err != nil || response.StatusCode != fiber.StatusCreated {
		t.Fatalf("create response=%v err=%v", response, err)
	}

	for _, test := range []struct {
		decision accessgate.Decision
		status   int
	}{
		{decision: accessgate.Blocked, status: fiber.StatusUnauthorized},
		{decision: accessgate.Unavailable, status: fiber.StatusServiceUnavailable},
	} {
		gate.decision = test.decision
		request := httptest.NewRequest(fiber.MethodGet, "/v1/go/gate-hop", nil)
		request.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
		response, err := app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != test.status || response.Header.Get(fiber.HeaderLocation) != "" {
			t.Fatalf("decision=%s status=%d location=%q", test.decision, response.StatusCode, response.Header.Get(fiber.HeaderLocation))
		}
	}
}

func TestInvalidShortLinkBearerIsRejectedBeforeGateLookup(t *testing.T) {
	t.Parallel()

	gate := &httpAccessGate{decision: accessgate.Allowed}
	app := memoryAppWithAccessGate(gate)
	request := httptest.NewRequest(fiber.MethodGet, "/v1/go/missing", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer invalid")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}
}
