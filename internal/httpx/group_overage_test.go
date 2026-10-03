package httpx_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// keycloakUserGroups is Keycloak's "a person's Groups" call over the memory
// directory: it counts the calls and fails while down is set.
type keycloakUserGroups struct {
	dir   *identity.Memory
	calls atomic.Int64
	down  atomic.Bool
}

func (k *keycloakUserGroups) GroupsForUser(ctx context.Context, id uuid.UUID) ([]identity.Group, error) {
	k.calls.Add(1)
	if k.down.Load() {
		return nil, errors.New("identity: keycloak user groups failed with status 503")
	}
	return k.dir.GroupsForUser(ctx, id)
}

type overageClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *overageClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *overageClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// overageEnv is the assembled app with core's Group overage cache over a
// counted Keycloak and an injected clock. person leads WEBLAB and is in more
// Groups than a token carries: their token holds the marker, not the list.
type overageEnv struct {
	app    *fiber.App
	keys   *testauth.Bundle
	kc     *keycloakUserGroups
	clock  *overageClock
	person uuid.UUID
}

const weblabLeaders = "g-weblab-l"

func newOverageEnv(t *testing.T) *overageEnv {
	t.Helper()
	keys := testauth.New(t)
	dir := identity.NewMemory()
	kc := &keycloakUserGroups{dir: dir}
	clock := &overageClock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	groups := identity.NewOverageGroups(kc, identity.OverageOptions{Now: clock.Now})
	deps := memoryDepsOver(dir, identity.Options{GroupCache: groups})
	deps.ParseToken = keys.Parse()
	deps.GroupOverage = groups

	person := uuid.New()
	dir.PutUser(identity.Person{ID: person, Email: "overage@example.test"})
	dir.PutGroup(identity.Group{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	dir.PutGroup(identity.Group{ID: weblabLeaders, Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"})
	if err := dir.AddMember(context.Background(), weblabLeaders, person); err != nil {
		t.Fatal(err)
	}
	return &overageEnv{app: httpx.New(deps), keys: keys, kc: kc, clock: clock, person: person}
}

// markedToken is the person's token as the Group overage mapper writes it.
func (e *overageEnv) markedToken(t *testing.T) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{
		"sub": e.person.String(), "email": "overage@example.test", "given_name": "O", "family_name": "V",
		"_claim_names":   map[string]any{"groups": "src1"},
		"_claim_sources": map[string]any{"src1": map[string]any{"endpoint": testauth.Issuer + "/sky-groups"}},
	})
}

func (e *overageEnv) adminToken(t *testing.T) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "email": "yk@example.test", "given_name": "Y", "family_name": "K",
		"groups": []string{"/UYELER/YK"},
	})
}

func (e *overageEnv) createEvent(t *testing.T, token, team string) jsonResponse {
	t.Helper()
	return sendJSON(t, e.app, token, fiber.MethodPost, "/v1/events",
		`{"name":"Hack","location":"YTÜ","ownerTeam":"`+team+`"}`)
}

func (e *overageEnv) wantLookups(t *testing.T, want int64) {
	t.Helper()
	if got := e.kc.calls.Load(); got != want {
		t.Fatalf("Keycloak asked %d times, want %d", got, want)
	}
}

// A token with the marker and no groups claim gets the team decisions of the
// person's Groups in Keycloak.
func TestGroupOverageTokenGetsItsTeamDecisionsFromKeycloak(t *testing.T) {
	t.Parallel()
	e := newOverageEnv(t)
	marked := e.markedToken(t)

	if got := e.createEvent(t, marked, "WEBLAB"); got.status != fiber.StatusCreated {
		t.Fatalf("WEBLAB leader creating a WEBLAB event: status %d body %v", got.status, got.body)
	}
	if got := e.createEvent(t, marked, "GAMELAB"); got.status != fiber.StatusForbidden {
		t.Fatalf("WEBLAB leader creating a GAMELAB event: status %d body %v", got.status, got.body)
	}
	e.wantLookups(t, 1)

	// The same person without the marker and without a groups claim: no
	// Groups, and nobody is asked.
	bare := e.keys.Token(t, jwt.MapClaims{"sub": e.person.String(), "email": "overage@example.test"})
	if got := e.createEvent(t, bare, "WEBLAB"); got.status != fiber.StatusForbidden {
		t.Fatalf("no marker, no groups: status %d body %v", got.status, got.body)
	}
	e.wantLookups(t, 1)
}

// Keycloak is asked once a minute per person, and at once after core itself
// changes the person's memberships.
func TestGroupOverageCacheAndMembershipWrites(t *testing.T) {
	t.Parallel()
	e := newOverageEnv(t)
	marked, admin := e.markedToken(t), e.adminToken(t)

	for range 2 {
		if got := e.createEvent(t, marked, "WEBLAB"); got.status != fiber.StatusCreated {
			t.Fatalf("status %d body %v", got.status, got.body)
		}
	}
	e.wantLookups(t, 1)
	e.clock.advance(61 * time.Second)
	if got := e.createEvent(t, marked, "WEBLAB"); got.status != fiber.StatusCreated {
		t.Fatalf("status %d body %v", got.status, got.body)
	}
	e.wantLookups(t, 2)

	// An admin removes the person from WEBLAB's leaders: their next request,
	// within the minute, is decided on the new Groups.
	if got := sendJSON(t, e.app, admin, fiber.MethodDelete, "/v1/groups/"+weblabLeaders+"/members/"+e.person.String(), ""); got.status != fiber.StatusNoContent {
		t.Fatalf("remove member: status %d body %v", got.status, got.body)
	}
	if got := e.createEvent(t, marked, "WEBLAB"); got.status != fiber.StatusForbidden {
		t.Fatalf("after removal: status %d body %v", got.status, got.body)
	}
	e.wantLookups(t, 3)

	if got := sendJSON(t, e.app, admin, fiber.MethodPost, "/v1/groups/"+weblabLeaders+"/members", `{"userId":"`+e.person.String()+`"}`); got.status != fiber.StatusNoContent {
		t.Fatalf("add member: status %d body %v", got.status, got.body)
	}
	if got := e.createEvent(t, marked, "WEBLAB"); got.status != fiber.StatusCreated {
		t.Fatalf("after adding back: status %d body %v", got.status, got.body)
	}
	e.wantLookups(t, 4)
}

// Keycloak cannot be reached: a marked token is refused with 503 and
// problem+json, never decided as a person without Groups; a token with its
// groups claim is not affected.
func TestGroupOverageFailsClosedWhenKeycloakIsDown(t *testing.T) {
	t.Parallel()
	e := newOverageEnv(t)
	e.kc.down.Store(true)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.markedToken(t))
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get(fiber.HeaderContentType); got != "application/problem+json" {
		t.Fatalf("content type %q body %s", got, body)
	}
	if !strings.Contains(string(body), `"status":503`) {
		t.Fatalf("body %s", body)
	}
	if resp.Header.Get(fiber.HeaderRetryAfter) != "1" || resp.Header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("headers %v", resp.Header)
	}
	// Reading the person's own profile needs no Group either, but every
	// request of a marked token waits for its Groups.
	if got := sendJSON(t, e.app, e.markedToken(t), fiber.MethodGet, "/v1/users/me", ""); got.status != fiber.StatusServiceUnavailable {
		t.Fatalf("GET /v1/users/me: status %d", got.status)
	}

	if got := e.createEvent(t, e.adminToken(t), "WEBLAB"); got.status != fiber.StatusCreated {
		t.Fatalf("a token with its groups claim while Keycloak is down: status %d body %v", got.status, got.body)
	}

	e.kc.down.Store(false)
	if got := e.createEvent(t, e.markedToken(t), "WEBLAB"); got.status != fiber.StatusCreated {
		t.Fatalf("Keycloak back: status %d body %v", got.status, got.body)
	}
}

// Without a Group overage cache wired, a marked token is refused rather than
// read as groupless.
func TestGroupOverageWithoutACacheRefusesMarkedTokens(t *testing.T) {
	t.Parallel()
	e := newOverageEnv(t)
	app := memoryApp(e.keys.Parse())
	if got := sendJSON(t, app, e.markedToken(t), fiber.MethodGet, "/v1/users/me", ""); got.status != fiber.StatusServiceUnavailable {
		t.Fatalf("status %d body %v", got.status, got.body)
	}
}

func TestGroupOverageMetricsAreServed(t *testing.T) {
	t.Parallel()
	e := newOverageEnv(t)
	if got := e.createEvent(t, e.markedToken(t), "WEBLAB"); got.status != fiber.StatusCreated {
		t.Fatalf("status %d body %v", got.status, got.body)
	}
	resp, err := e.app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/metrics", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{"skylab_group_overage_requests_total 1\n", "skylab_group_overage_people 1\n"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), e.person.String()) {
		t.Fatalf("metrics name the person:\n%s", body)
	}
}
