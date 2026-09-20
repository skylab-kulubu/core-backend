package httpx_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var (
	testPassKey     *ecdsa.PrivateKey
	testPassKeyOnce sync.Once
)

func testPassSigner() *skypass.Signer {
	testPassKeyOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		testPassKey = key
	})
	return skypass.NewSigner(testPassKey, skypass.DefaultTTL)
}

func memoryApp(parse ...func(string) (authn.Identity, error)) *fiber.App {
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	users := user.NewMemoryStore()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	dir := identity.NewMemory()
	deps := httpx.Deps{
		Users:       user.NewService(users),
		Identity:    identity.NewService(dir, users, az),
		Events:      event.NewService(events, az),
		Seasons:     season.NewService(season.NewMemoryStore(), az),
		Tickets:     ticket.NewService(tickets, events, az, users, dir),
		Competitors: competitor.NewService(competitor.NewMemoryStore(events), events, az),
		Media:       media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), az, ""),
		URLs:        shorturl.NewService(shorturl.NewMemoryStore(), az),
		Certificates: certificate.NewService(
			certificate.NewMemoryStore(), tickets, events, users, az, nil, nil, "https://api.example.test",
		),
		SkyPass: skypass.NewService(users, az, testPassSigner()),
		URLAttributionGuard: func(ctx context.Context, id uuid.UUID) bool {
			allowed, err := users.CanAttribute(ctx, id)
			return err == nil && allowed
		},
	}
	if len(parse) > 0 {
		deps.ParseToken = parse[0]
	}
	return httpx.New(deps)
}

func TestCertificateShortLinkProxyRoute(t *testing.T) {
	t.Setenv("CERTIFICATE_PUBLIC_PAGE_ORIGIN", "https://yildizskylab.com/sertifika")
	app := memoryApp()
	const serial = "75E614C7A33C08CB5C04804D6C24F9E7"

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/c/"+serial, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("redirect %d body %s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get(fiber.HeaderLocation), "https://yildizskylab.com/sertifika/"+serial; got != want {
		t.Fatalf("location %q want %q", got, want)
	}
}

func TestHealthAnonymous(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/health", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestLifecycleRestoreRoutesAreRegistered(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	id := uuid.NewString()
	paths := []string{
		"/v1/events/" + id + "/restore",
		"/v1/event-days/" + id + "/restore",
		"/v1/sessions/" + id + "/restore",
		"/v1/seasons/" + id + "/restore",
		"/v1/competitors/" + id + "/reinstate",
		"/v1/media/" + id + "/restore",
		"/v1/urls/" + id + "/restore",
	}
	for _, path := range paths {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("POST %s status = %d, want %d", path, resp.StatusCode, fiber.StatusUnauthorized)
		}
	}
}

func TestBearerGroupsReachMe(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": id.String(), "email": "yk@example.com", "given_name": "Y", "family_name": "K", "groups": []string{"/UYELER/YK"},
	}))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var got user.User
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Email != "yk@example.com" {
		t.Fatalf("got %+v", got)
	}
}

func TestInvalidBearerIs401Problem(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("content-type %s", ct)
	}
}

func TestUnsetJWKSRejectsAnyBearer(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	keys := testauth.New(t)
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{"sub": id.String(), "email": "yk@example.com"}))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestVerifiedBearerAcceptsSignedJWT(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{"sub": id.String(), "email": "yk@example.com"}))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestVerifiedBearerRejectsUnsigned(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	header := "eyJhbGciOiJub25lIn0"
	body := "eyJzdWIiOiIxMTExMTExMS0xMTExLTExMTEtMTExMS0xMTExMTExMTExMTEiLCJhdWQiOiJjb3JlIn0"
	_ = id
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+header+"."+body+".x")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestVerifiedBearerRejectsWrongAudience(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{"sub": id.String(), "aud": "account"}))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var problem map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem["title"] == nil || problem["status"] == nil || problem["type"] == nil || problem["detail"] == nil || problem["instance"] == nil {
		t.Fatalf("problem %+v", problem)
	}
}

func TestVerifiedBearerRejectsWrongIssuer(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": id.String(), "iss": "https://other.example/realms/e-skylab",
	}))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestVerifiedBearerRejectsMissingAudience(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := keys.Sign(t, jwt.MapClaims{
		"sub": id.String(),
		"iss": keys.Issuer,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestLeaderboardUsesTeamPathNotType(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/leaderboard/type/WEBLAB", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("old type path %d", resp.StatusCode)
	}
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/leaderboard/team/WEBLAB", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("team path %d", resp.StatusCode)
	}
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/leaderboard/season/"+uuid.NewString()+"/type/WEBLAB", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("old season type path %d", resp.StatusCode)
	}
}

func TestCheckInUsesSessionPathNotEventDay(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	id := uuid.New()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/v1/tickets/"+id.String()+"/event-days/"+id.String()+"/check-in", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("old event-day path %d", resp.StatusCode)
	}
	resp, err = app.Test(httptest.NewRequest(fiber.MethodPost, "/v1/tickets/"+id.String()+"/sessions/"+id.String()+"/check-in", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("session path %d", resp.StatusCode)
	}
	resp, err = app.Test(httptest.NewRequest(fiber.MethodPost, "/v1/sessions/"+id.String()+"/check-in/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("me path %d", resp.StatusCode)
	}
	req := httptest.NewRequest(fiber.MethodPost, "/v1/sessions/"+id.String()+"/check-in/guest", strings.NewReader(`{"email":"ada@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("guest path %d", resp.StatusCode)
	}
	resp, err = app.Test(httptest.NewRequest(fiber.MethodPost, "/v1/sessions/"+id.String()+"/check-in/skypass", strings.NewReader(`{"uid":"04AABBCCDD"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("skypass path %d", resp.StatusCode)
	}
}

func TestGuestApplyWithInvalidBearerStillCreatesTicket(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	admin := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"GECEKODU","location":"YTÜ","ownerTeam":"GECEKODU"}`,
	))
	admin.Header.Set("Content-Type", "application/json")
	admin.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": "11111111-1111-1111-1111-111111111111", "email": "yk@example.com",
		"groups": []string{"/UYELER/YK"},
	}))
	created, err := app.Test(admin)
	if err != nil {
		t.Fatal(err)
	}
	if created.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(created.Body)
		t.Fatalf("create event %d body %s", created.StatusCode, body)
	}
	var ev event.Event
	if err := json.NewDecoder(created.Body).Decode(&ev); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/applications/guest", strings.NewReader(
		`{"firstName":"Yusuf","lastName":"Acmaci","email":"yusuf@example.com"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("guest apply with bad bearer %d body %s", resp.StatusCode, body)
	}

	list := httptest.NewRequest(fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/tickets", nil)
	list.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": "11111111-1111-1111-1111-111111111111", "email": "yk@example.com",
		"groups": []string{"/UYELER/YK"},
	}))
	listed, err := app.Test(list)
	if err != nil {
		t.Fatal(err)
	}
	if listed.StatusCode != fiber.StatusOK {
		t.Fatalf("list tickets %d", listed.StatusCode)
	}
	var tickets []ticket.Ticket
	if err := json.NewDecoder(listed.Body).Decode(&tickets); err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 || tickets[0].TicketType != ticket.Guest || tickets[0].GuestEmail != "yusuf@example.com" {
		t.Fatalf("roster %+v", tickets)
	}
}

func TestSkyPassJWKSAnonymous(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/skypass/jwks", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("jwks %d", resp.StatusCode)
	}
	var doc skypass.JWKS
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Keys) != 1 || doc.Keys[0].Kty != "EC" || doc.Keys[0].Alg != "ES256" || doc.Keys[0].X == "" || doc.Keys[0].Y == "" {
		t.Fatalf("jwks %+v", doc)
	}
}

func TestGoRedirectRecordsHitsWithoutRequiringLogin(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	admin := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	clicker := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	create := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": admin.String(), "email": "yk@example.com", "groups": []string{"/UYELER/YK"},
	}))
	resp, err := app.Test(create)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %d %s", resp.StatusCode, body)
	}
	var created shorturl.URL
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	anon := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	anon.Header.Set("User-Agent", "WhatsApp/2.0")
	anon.Header.Set("X-Forwarded-For", "203.0.113.9")
	redir, err := app.Test(anon)
	if err != nil {
		t.Fatal(err)
	}
	if redir.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("anon %d", redir.StatusCode)
	}

	bad := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	bad.Header.Set("Authorization", "Bearer not-a-jwt")
	badResp, err := app.Test(bad)
	if err != nil {
		t.Fatal(err)
	}
	if badResp.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("invalid bearer hop %d", badResp.StatusCode)
	}

	authed := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	authed.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{"sub": clicker.String()}))
	if _, err := app.Test(authed); err != nil {
		t.Fatal(err)
	}

	cookie := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	cookie.AddCookie(&http.Cookie{Name: "access_token", Value: keys.Token(t, jwt.MapClaims{"sub": clicker.String()})})
	if _, err := app.Test(cookie); err != nil {
		t.Fatal(err)
	}

	qrResp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/club/qr?logo=1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if qrResp.StatusCode != fiber.StatusOK {
		t.Fatalf("qr %d", qrResp.StatusCode)
	}

	list := httptest.NewRequest(fiber.MethodGet, "/v1/urls/"+created.ID.String()+"/hits", nil)
	list.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": admin.String(), "groups": []string{"/UYELER/YK"},
	}))
	hitsResp, err := app.Test(list)
	if err != nil {
		t.Fatal(err)
	}
	if hitsResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(hitsResp.Body)
		t.Fatalf("hits %d %s", hitsResp.StatusCode, body)
	}
	var hits []shorturl.Hit
	if err := json.NewDecoder(hitsResp.Body).Decode(&hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 4 {
		t.Fatalf("hits %+v", hits)
	}
	if hits[0].UserID != nil {
		t.Fatalf("cookie hop should stay anonymous %+v", hits[0])
	}
	if hits[1].UserID == nil || *hits[1].UserID != clicker {
		t.Fatalf("bearer %+v", hits[1])
	}
	if hits[2].UserID != nil || hits[3].UserID != nil {
		t.Fatalf("public hops should be anonymous %+v", hits)
	}
	if hits[3].IP != "203.0.113.9" || hits[3].UserAgent != "WhatsApp/2.0" {
		t.Fatalf("anon meta %+v", hits[3])
	}
}

func TestHitsListAuthThroughJWT(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	owner := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mod := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	create := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": owner.String(),
		"resource_access": map[string]any{
			"core": map[string]any{"roles": []any{"url:create", "url:access"}},
		},
	}))
	resp, err := app.Test(create)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %d %s", resp.StatusCode, body)
	}
	var created shorturl.URL
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	path := "/v1/urls/" + created.ID.String() + "/hits"

	member := httptest.NewRequest(fiber.MethodGet, path, nil)
	member.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": owner.String(),
		"resource_access": map[string]any{
			"core": map[string]any{"roles": []any{"url:access"}},
		},
	}))
	memberResp, err := app.Test(member)
	if err != nil {
		t.Fatal(err)
	}
	if memberResp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("member %d", memberResp.StatusCode)
	}

	modReq := httptest.NewRequest(fiber.MethodGet, path, nil)
	modReq.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": mod.String(),
		"resource_access": map[string]any{
			"core": map[string]any{"roles": []any{"url:moderator"}},
		},
	}))
	modResp, err := app.Test(modReq)
	if err != nil {
		t.Fatal(err)
	}
	if modResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(modResp.Body)
		t.Fatalf("moderator %d %s", modResp.StatusCode, body)
	}

	ykReq := httptest.NewRequest(fiber.MethodGet, path, nil)
	ykReq.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub":    uuid.MustParse("22222222-2222-2222-2222-222222222222").String(),
		"groups": []string{"/UYELER/YK"},
	}))
	ykResp, err := app.Test(ykReq)
	if err != nil {
		t.Fatal(err)
	}
	if ykResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(ykResp.Body)
		t.Fatalf("privileged %d %s", ykResp.StatusCode, body)
	}
}
