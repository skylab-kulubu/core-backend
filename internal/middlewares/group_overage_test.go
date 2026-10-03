package middlewares

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

type stubGroups struct {
	paths  []string
	cached bool
	err    error
	asked  []uuid.UUID
}

func (s *stubGroups) Paths(_ context.Context, id uuid.UUID) ([]string, bool, error) {
	s.asked = append(s.asked, id)
	if s.err != nil {
		return nil, false, s.err
	}
	return slices.Clone(s.paths), s.cached, nil
}

const overagePerson = "11111111-1111-1111-1111-111111111111"

// overageApp puts ident where the Bearer middleware would and answers the
// Groups the next handler sees.
func overageApp(groups GroupResolver, logger *log.Logger, ident authn.Identity) *fiber.App {
	app := fiber.New()
	app.Use(requestid.New())
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Use(groupOverage(groups, logger))
	app.Get("/", func(c fiber.Ctx) error {
		seen, _ := c.Locals(authn.LocalsIdentity).(authn.Identity)
		return c.JSON(seen.Groups)
	})
	return app
}

func call(t *testing.T, app *fiber.App) (int, []string) {
	t.Helper()
	request := httptest.NewRequest(fiber.MethodGet, "/", nil)
	request.Header.Set(fiber.HeaderXRequestID, "correlation-123")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	var groups []string
	if response.StatusCode == fiber.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&groups); err != nil {
			t.Fatal(err)
		}
	}
	_ = response.Body.Close()
	return response.StatusCode, groups
}

func TestGroupOverageFillsInTheGroupsOfAMarkedToken(t *testing.T) {
	t.Parallel()
	person := uuid.MustParse(overagePerson)
	stub := &stubGroups{paths: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	app := overageApp(stub, log.New(&bytes.Buffer{}, "", 0), authn.Identity{ID: person, GroupOverage: true})

	status, groups := call(t, app)
	if status != fiber.StatusOK || !slices.Equal(groups, []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}) {
		t.Fatalf("status %d groups %q", status, groups)
	}
	if !slices.Equal(stub.asked, []uuid.UUID{person}) {
		t.Fatalf("asked for %v, want the token's subject", stub.asked)
	}
}

// A token with its groups claim keeps working as before: nobody is asked.
func TestGroupOverageLeavesAnUnmarkedTokenAlone(t *testing.T) {
	t.Parallel()
	for name, ident := range map[string]authn.Identity{
		"groups in the token": {ID: uuid.MustParse(overagePerson), Groups: []string{"/UYELER/YK"}},
		"no groups at all":    {ID: uuid.MustParse(overagePerson)},
		"no sign-in":          {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := &stubGroups{err: errors.New("must not be asked")}
			status, groups := call(t, overageApp(stub, log.New(&bytes.Buffer{}, "", 0), ident))
			if status != fiber.StatusOK || !slices.Equal(groups, ident.Groups) {
				t.Fatalf("status %d groups %q, want %q", status, groups, ident.Groups)
			}
			if len(stub.asked) != 0 {
				t.Fatalf("asked for %v", stub.asked)
			}
		})
	}
}

func TestGroupOverageRefusesWhenTheGroupsCannotBeRead(t *testing.T) {
	t.Parallel()
	person := uuid.MustParse(overagePerson)
	for name, groups := range map[string]GroupResolver{
		"Keycloak down": &stubGroups{err: fmt.Errorf("%w: identity: keycloak user groups failed with status 503", identity.ErrGroupsUnavailable)},
		"not wired":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			app := overageApp(groups, log.New(&output, "", 0), authn.Identity{ID: person, GroupOverage: true})
			request := httptest.NewRequest(fiber.MethodGet, "/", nil)
			request.Header.Set(fiber.HeaderXRequestID, "correlation-123")
			response, err := app.Test(request)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != fiber.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503", response.StatusCode)
			}
			if response.Header.Get(fiber.HeaderRetryAfter) != "1" || response.Header.Get(fiber.HeaderCacheControl) != "no-store" {
				t.Fatalf("headers %v", response.Header)
			}
			got := output.String()
			for _, want := range []string{`"event":"group_overage"`, `"correlation_id":"correlation-123"`, `"outcome":"unavailable"`} {
				if !strings.Contains(got, want) {
					t.Fatalf("log missing %q: %s", want, got)
				}
			}
			if strings.Contains(got, person.String()) {
				t.Fatalf("log names the person: %s", got)
			}
		})
	}
}

// Keycloak does not know the token's subject: the token speaks for nobody.
func TestGroupOverageRefusesAPersonKeycloakDoesNotKnow(t *testing.T) {
	t.Parallel()
	person := uuid.MustParse(overagePerson)
	var output bytes.Buffer
	app := overageApp(&stubGroups{err: identity.ErrNotFound}, log.New(&output, "", 0), authn.Identity{ID: person, GroupOverage: true})
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d, want 401", response.StatusCode)
	}
	if got := response.Header.Get(fiber.HeaderWWWAuthenticate); got != `Bearer error="invalid_token"` {
		t.Fatalf("WWW-Authenticate %q", got)
	}
	if got := output.String(); !strings.Contains(got, `"outcome":"unknown_subject"`) || strings.Contains(got, person.String()) {
		t.Fatalf("log %s", got)
	}
}

// A lookup is logged with how many paths came back, never which; an answer
// from memory is not logged.
func TestGroupOverageLogsLookupsWithoutPersonalData(t *testing.T) {
	t.Parallel()
	person := uuid.MustParse(overagePerson)
	paths := []string{"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/GAMELAB"}
	var output bytes.Buffer
	stub := &stubGroups{paths: paths}
	app := overageApp(stub, log.New(&output, "", 0), authn.Identity{ID: person, GroupOverage: true})
	if status, _ := call(t, app); status != fiber.StatusOK {
		t.Fatalf("status %d", status)
	}
	got := output.String()
	for _, want := range []string{`"event":"group_overage"`, `"correlation_id":"correlation-123"`, `"outcome":"fetched"`, `"paths":2`} {
		if !strings.Contains(got, want) {
			t.Fatalf("log missing %q: %s", want, got)
		}
	}
	for _, forbidden := range append([]string{person.String(), "UYELER"}, paths...) {
		if strings.Contains(got, forbidden) {
			t.Fatalf("log exposes %q: %s", forbidden, got)
		}
	}

	output.Reset()
	stub.cached = true
	if status, _ := call(t, app); status != fiber.StatusOK {
		t.Fatalf("status %d", status)
	}
	if output.Len() != 0 {
		t.Fatalf("an answer from memory was logged: %s", output.String())
	}
}
