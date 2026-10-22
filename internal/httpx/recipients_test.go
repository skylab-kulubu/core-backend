package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const recipientsResolvePath = "/internal/v1/audiences/recipients:resolve"

// downAddresses is a Keycloak whose user reads fail.
type downAddresses struct{}

func (downAddresses) AddressHoldersPage(context.Context, int, int) (identity.AddressHolderPage, error) {
	return identity.AddressHolderPage{}, errors.New("identity: address directory read failed with status 502")
}

func (downAddresses) IdentityProviderLinkedPage(context.Context, string, int, int) (identity.LinkedPage, error) {
	return identity.LinkedPage{}, errors.New("identity: identity provider link read failed with status 502")
}

type recipientsEnv struct {
	app    *fiber.App
	keys   *testauth.Bundle
	person uuid.UUID
	prefs  *commpref.Service
}

// newRecipientsEnv is the app with one person in WEBLAB, uye@example.test
// (verified), whose School e-mail okul@std.yildiz.edu.tr is linked to YTÜ.
// keyed false leaves the preferences off; down makes Keycloak's user
// reads fail.
func newRecipientsEnv(t *testing.T, keyed, down bool) recipientsEnv {
	t.Helper()
	keys := testauth.New(t)
	dir := identity.NewMemory()
	for _, g := range []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: map[string]string{"public_listing": "true"}},
	} {
		dir.PutGroup(g)
	}
	person := uuid.New()
	dir.PutUser(identity.Person{ID: person, Email: "uye@example.test", SchoolEmail: "Okul@STD.yildiz.edu.tr", FirstName: "Ad", LastName: "Soyad"})
	dir.LinkIdentityProvider(commpref.DefaultYTUIdentityProvider, person)
	if err := dir.AddMember(context.Background(), "g-weblab", person); err != nil {
		t.Fatal(err)
	}
	deps := memoryDepsOver(dir, identity.Options{})
	deps.ParseToken = keys.Parse()
	config := commpref.Config{}
	if keyed {
		config = commpref.TestConfig(bytes.Repeat([]byte{7}, 32), "https://api.example.test")
	}
	prefs := commpref.NewService(config, commpref.NewMemoryStore(), dir, authz.DefaultPrivilegedRoots())
	deps.CommunicationPreferences = prefs
	var addresses commpref.AddressDirectory = dir
	if down {
		addresses = downAddresses{}
	}
	deps.Recipients = commpref.NewRecipients(prefs, addresses, user.NewMemoryStore(), commpref.DefaultYTUIdentityProvider)
	return recipientsEnv{app: httpx.New(deps), keys: keys, person: person, prefs: prefs}
}

func (e recipientsEnv) service(t *testing.T, roles ...string) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "azp": "skymail", "client_id": "skymail",
		"resource_access": map[string]any{"core": map[string]any{"roles": roles}},
	})
}

// SkyMail's question as its client sends it: the colon is literal, one
// answer per recipient in the order asked, "reason": null for an allowed
// person, {"matched": false} alone for an address without an account.
func TestRecipientsResolveRoute(t *testing.T) {
	env := newRecipientsEnv(t, true, false)
	body := fmt.Sprintf(`{"channel":"email","category":"team:g-weblab:duyuru","recipients":[{"email":"dis@example.com"},{"email":" OKUL@std.yildiz.edu.tr"},{"sub":%q}]}`, env.person)
	got := audienceCall(t, env.app, env.service(t, commpref.AudienceRole), fiber.MethodPost, recipientsResolvePath, body)
	if got.status != fiber.StatusOK || got.header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	var answer struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(got.raw, &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Results) != 3 || len(answer.Results[0]) != 1 || answer.Results[0]["matched"] != false {
		t.Fatalf("answer %s", got.raw)
	}
	for _, r := range answer.Results[1:] {
		link, _ := r["unsubscribeUrl"].(string)
		if r["matched"] != true || r["sub"] != env.person.String() || r["allowed"] != true || r["reason"] != nil ||
			!strings.HasPrefix(link, "https://api.example.test/v1/communication-preferences/unsubscribe?t=") {
			t.Fatalf("answer %s", got.raw)
		}
		if _, ok := r["reason"]; !ok {
			t.Fatalf("no reason field: %s", got.raw)
		}
	}
	// The link closes the category: the next answer is opted_out, without
	// a link.
	link := answer.Results[1]["unsubscribeUrl"].(string)
	token := link[strings.Index(link, "?t=")+3:]
	req := httptest.NewRequest(fiber.MethodPost, "/v1/communication-preferences/unsubscribe?t="+token, strings.NewReader("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := env.app.Test(req, fiber.TestConfig{Timeout: 0}); err != nil || resp.StatusCode >= 300 {
		t.Fatalf("unsubscribe: %v %v", resp, err)
	}
	again := audienceCall(t, env.app, env.service(t, commpref.AudienceRole), fiber.MethodPost, recipientsResolvePath,
		fmt.Sprintf(`{"channel":"email","category":"team:g-weblab:duyuru","recipients":[{"sub":%q}]}`, env.person))
	if again.status != fiber.StatusOK || !strings.Contains(string(again.raw), `"reason":"opted_out"`) || strings.Contains(string(again.raw), "unsubscribeUrl") {
		t.Fatalf("again: %d %s", again.status, again.raw)
	}
	other := audienceCall(t, env.app, env.service(t, commpref.AudienceRole), fiber.MethodPost, recipientsResolvePath,
		fmt.Sprintf(`{"channel":"email","category":"team:g-weblab:haber","recipients":[{"sub":%q}]}`, env.person))
	if other.status != fiber.StatusOK || !strings.Contains(string(other.raw), `"allowed":true`) {
		t.Fatalf("haber: %d %s", other.status, other.raw)
	}
	// The colon is no parameter: another verb is no route.
	if verb := audienceCall(t, env.app, env.service(t, commpref.AudienceRole), fiber.MethodPost, "/internal/v1/audiences/recipients:other", body); verb.status != fiber.StatusNotFound {
		t.Fatalf("other verb: %d %s", verb.status, verb.raw)
	}
}

// Only a service account with audience:read; a person's token is refused
// even with the role, communication-preferences:read is not enough, and no
// token is 401.
func TestRecipientsResolveAuthorization(t *testing.T) {
	env := newRecipientsEnv(t, true, false)
	body := `{"channel":"email","category":"club:duyuru","recipients":[{"email":"uye@example.test"}]}`
	personWithRole := env.keys.Token(t, jwt.MapClaims{
		"sub": env.person.String(), "azp": "admin", "groups": []string{"/YK"},
		"resource_access": map[string]any{"core": map[string]any{"roles": []string{commpref.AudienceRole}}},
	})
	for name, c := range map[string]struct {
		token, code string
	}{
		"person":           {groupToken(t, env.keys, env.person.String(), "/UYELER/ARGE/WEBLAB"), "recipients_resolve_service_only"},
		"person with role": {personWithRole, "recipients_resolve_service_only"},
		"service tracking": {env.service(t, commpref.TrackingResolveRole), "recipients_resolve_role_missing"},
		"service no role":  {env.service(t), "recipients_resolve_role_missing"},
	} {
		got := audienceCall(t, env.app, c.token, fiber.MethodPost, recipientsResolvePath, body)
		if got.status != fiber.StatusForbidden || !strings.Contains(string(got.raw), c.code) || strings.Contains(string(got.raw), "matched") {
			t.Fatalf("%s: %d %s", name, got.status, got.raw)
		}
	}
	if got := audienceCall(t, env.app, "", fiber.MethodPost, recipientsResolvePath, body); got.status != fiber.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s", got.status, got.raw)
	}
}

// Through the public edge (a forwarding header) the route is a bare 404
// before its token is read.
func TestRecipientsResolveIsNotOnTheEdge(t *testing.T) {
	env := newRecipientsEnv(t, true, false)
	body := `{"channel":"email","category":"club:duyuru","recipients":[{"email":"uye@example.test"}]}`
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip"} {
		for _, token := range []string{env.service(t, commpref.AudienceRole), ""} {
			req := httptest.NewRequest(fiber.MethodPost, recipientsResolvePath, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			req.Header.Set(header, "203.0.113.7")
			resp, err := env.app.Test(req, fiber.TestConfig{Timeout: 0})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != fiber.StatusNotFound || strings.Contains(string(raw), "matched") {
				t.Fatalf("%s token=%v: %d %s", header, token != "", resp.StatusCode, raw)
			}
		}
	}
}

func TestRecipientsResolveRefusals(t *testing.T) {
	env := newRecipientsEnv(t, true, false)
	skymail := env.service(t, commpref.AudienceRole)
	id := uuid.New().String()
	many := make([]string, commpref.MaxRecipients+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"email":"a%d@example.com"}`, i)
	}
	one := `[{"email":"uye@example.test"}]`
	for _, c := range []struct {
		body, code string
	}{
		{``, "recipients_resolve_invalid"},
		{`[]`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru"}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":null}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[]}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[` + strings.Join(many, ",") + `]}`, "recipients_resolve_invalid"},
		{`{"channel":"sms","category":"club:duyuru","recipients":` + one + `}`, "recipients_resolve_invalid"},
		{`{"category":"club:duyuru","recipients":` + one + `}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"all","recipients":` + one + `}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"tracking","recipients":` + one + `}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:bulten","recipients":` + one + `}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":` + one + `,"x":1}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[{"email":"a@example.com","sub":"` + id + `"}]}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[{}]}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[{"email":"a@example.com","name":"A"}]}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[{"email":"a@example.com"},{"email":"A@example.com "}]}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[{"sub":"` + id + `"},{"sub":"` + id + `"}]}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[{"sub":"` + strings.ToUpper(id) + `"}]}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[{"sub":"00000000-0000-0000-0000-000000000000"}]}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"club:duyuru","recipients":[{"email":"yok"}]}`, "recipients_resolve_invalid"},
		{`{"channel":"email","category":"team:g-arge:duyuru","recipients":` + one + `}`, "recipients_resolve_category_unknown"},
		{`{"channel":"email","category":"team:yok:haber","recipients":` + one + `}`, "recipients_resolve_category_unknown"},
	} {
		got := audienceCall(t, env.app, skymail, fiber.MethodPost, recipientsResolvePath, c.body)
		if got.status != fiber.StatusBadRequest || !strings.Contains(string(got.raw), c.code) || got.header.Get(fiber.HeaderCacheControl) != "no-store" {
			t.Fatalf("%.100s: %d %s", c.body, got.status, got.raw)
		}
	}
	body := `{"channel":"email","category":"club:duyuru","recipients":` + one + `}`
	off := newRecipientsEnv(t, false, false)
	if got := audienceCall(t, off.app, off.service(t, commpref.AudienceRole), fiber.MethodPost, recipientsResolvePath, body); got.status != fiber.StatusServiceUnavailable ||
		!strings.Contains(string(got.raw), "communication_preferences_unavailable") {
		t.Fatalf("off: %d %s", got.status, got.raw)
	}
	down := newRecipientsEnv(t, true, true)
	got := audienceCall(t, down.app, down.service(t, commpref.AudienceRole), fiber.MethodPost, recipientsResolvePath, body)
	if got.status != fiber.StatusServiceUnavailable || !strings.Contains(string(got.raw), "recipients_resolve_directory_unavailable") ||
		got.header.Get(fiber.HeaderRetryAfter) == "" {
		t.Fatalf("down: %d %s %v", got.status, got.raw, got.header)
	}
}
