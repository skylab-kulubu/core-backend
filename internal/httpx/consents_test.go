package httpx_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/consent"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type consentMails struct {
	mu   sync.Mutex
	vars []map[string]string
}

func (m *consentMails) ConsentConfirmation(_ context.Context, _, _, _ string, vars map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vars = append(m.vars, vars)
}

func (m *consentMails) last(t *testing.T) map[string]string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.vars) == 0 {
		t.Fatal("no confirmation mail")
	}
	return m.vars[len(m.vars)-1]
}

type consentEnv struct {
	app     *fiber.App
	keys    *testauth.Bundle
	pool    *pgxpool.Pool
	mails   *consentMails
	eventID string
}

// newConsentEnv is the assembled app over PostgreSQL for the stores the
// consents touch: users, Events, Tickets and the consents themselves.
func newConsentEnv(t *testing.T, enabled bool) consentEnv {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	keys := testauth.New(t)
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	dir := identity.NewMemory()
	users := user.NewPostgresStore(pool)
	events := event.NewPostgresStore(pool)
	tickets := ticket.NewPostgresStore(pool)
	deps := memoryDepsOver(dir, identity.Options{})
	deps.ParseToken = keys.Parse()
	deps.Users = user.NewService(users, dir)
	deps.Events = event.NewService(events, az)
	deps.Tickets = ticket.NewService(tickets, events, az, users, dir)
	mails := &consentMails{}
	if enabled {
		svc := consent.NewService(pool, consent.TestConfig(bytes.Repeat([]byte{9}, 32), "https://api.example.test"), mails)
		svc.SetAsync(func(fn func()) { fn() })
		deps.Consents = svc
	}
	app := httpx.New(deps)
	created := sendJSON(t, app, groupToken(t, keys, ykSub, "/UYELER/YK"), fiber.MethodPost, "/v1/events",
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("create event %d %v", created.status, created.body)
	}
	return consentEnv{app: app, keys: keys, pool: pool, mails: mails, eventID: created.body["id"].(string)}
}

func (e consentEnv) serviceToken(t *testing.T, client string, roles ...string) string {
	t.Helper()
	return e.keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "azp": client, "client_id": client,
		"resource_access": map[string]any{"core": map[string]any{"roles": roles}},
	})
}

func (e consentEnv) count(t *testing.T, where string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM contact_consents WHERE `+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type rawAnswer struct {
	status int
	header http.Header
	body   string
}

func (e consentEnv) form(t *testing.T, method, target, body string) rawAnswer {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return rawAnswer{status: resp.StatusCode, header: resp.Header, body: string(raw)}
}

func pathOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

func tokenIn(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("token")
}

func TestGuestApplyConsentIsUntickedByDefaultAndConfirmedByLink(t *testing.T) {
	env := newConsentEnv(t, true)
	apply := func(body string) jsonResponse {
		return sendJSON(t, env.app, "", fiber.MethodPost, "/v1/events/"+env.eventID+"/applications/guest", body)
	}

	// No consents field: nothing recorded.
	if got := apply(`{"firstName":"A","lastName":"B","email":"plain@example.com"}`); got.status != fiber.StatusCreated {
		t.Fatalf("plain apply %d %v", got.status, got.body)
	}
	for _, bad := range []string{
		`{"firstName":"A","lastName":"B","email":"x@example.com","consents":["newsletter"]}`,
		`{"firstName":"A","lastName":"B","email":"x@example.com","consents":["recruitment_pool"]}`,
		`{"firstName":"A","lastName":"B","email":"x@example.com","consents":[{"purpose":"event_invitations","textVersion":"davet-v9"}]}`,
		`{"firstName":"A","lastName":"B","email":"x@example.com","consents":"event_invitations"}`,
	} {
		if got := apply(bad); got.status != fiber.StatusBadRequest {
			t.Fatalf("%s: %d %v", bad, got.status, got.body)
		}
	}
	var tickets int
	if err := env.pool.QueryRow(context.Background(), `SELECT count(*) FROM tickets WHERE guest_email = 'x@example.com'`).Scan(&tickets); err != nil || tickets != 0 {
		t.Fatalf("a refused consent still wrote a Ticket: %d %v", tickets, err)
	}

	got := apply(`{"firstName":"Ada","lastName":"L","email":"Ada@Example.com","consents":["event_invitations"]}`)
	if got.status != fiber.StatusCreated || got.body["status"] != "applied" || len(got.body) != 1 {
		t.Fatalf("apply with consent %d %v", got.status, got.body)
	}
	if env.count(t, `email = 'ada@example.com' AND source = 'guest_apply' AND confirmed_at IS NULL AND event_id IS NOT NULL`) != 1 ||
		env.count(t, `true`) != 1 {
		t.Fatal("pending grant not recorded")
	}
	mail := env.mails.last(t)

	page := env.form(t, fiber.MethodGet, pathOf(t, mail["confirmUrl"]), "")
	if page.status != fiber.StatusOK || !strings.Contains(page.body, `<form method="post" action="/v1/consents/confirm">`) ||
		page.header.Get("Referrer-Policy") != "no-referrer" || page.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("confirm page %d %v %s", page.status, page.header, page.body)
	}
	if env.count(t, `confirmed_at IS NOT NULL`) != 0 {
		t.Fatal("opening the page confirmed")
	}
	done := env.form(t, fiber.MethodPost, consent.ConfirmPath, "token="+url.QueryEscape(tokenIn(t, mail["confirmUrl"])))
	if done.status != fiber.StatusOK || !strings.Contains(done.body, "Onayın kaydedildi") {
		t.Fatalf("confirm %d %s", done.status, done.body)
	}
	if env.count(t, `confirmed_via = 'link'`) != 1 {
		t.Fatal("not confirmed")
	}
}

func TestConsentServiceRoutesNeedTheirRoleAndTheWithdrawLinkIsOneClick(t *testing.T) {
	env := newConsentEnv(t, true)
	record := env.serviceToken(t, "place", consent.RoleRecord)
	skymail := env.serviceToken(t, "skymail", consent.RoleAudienceRead)

	for name, token := range map[string]string{
		"person":          groupToken(t, env.keys, memberSub),
		"service no role": env.serviceToken(t, "place"),
		"wrong role":      skymail,
		"unmapped client": env.serviceToken(t, "unknown-app", consent.RoleRecord),
	} {
		got := sendJSON(t, env.app, token, fiber.MethodPost, "/v1/consents", `{"purpose":"event_invitations","email":"a@example.com"}`)
		if got.status != fiber.StatusForbidden {
			t.Errorf("%s: %d %v", name, got.status, got.body)
		}
	}
	got := sendJSON(t, env.app, record, fiber.MethodPost, "/v1/consents",
		`{"purpose":"event_invitations","textVersion":"davet-v1","email":"a@example.com","emailVerified":true}`)
	if got.status != fiber.StatusCreated || got.body["status"] != "active" {
		t.Fatalf("record %d %v", got.status, got.body)
	}
	if env.count(t, `source = 'place' AND client_id = 'place' AND confirmed_via = 'service'`) != 1 {
		t.Fatal("service grant not recorded")
	}
	got = sendJSON(t, env.app, record, fiber.MethodPost, "/v1/consents", `{"purpose":"event_invitations","email":"a@example.com","emailVerified":true}`)
	if got.status != fiber.StatusOK || got.body["status"] != "active" {
		t.Fatalf("record again %d %v", got.status, got.body)
	}
	if got := sendJSON(t, env.app, record, fiber.MethodPost, "/v1/consents", `{"purpose":"recruitment_pool","email":"a@example.com"}`); got.status != fiber.StatusBadRequest {
		t.Fatalf("place recorded a recruitment pool grant: %d", got.status)
	}
	forms := env.serviceToken(t, "forms", consent.RoleRecord)
	if got := sendJSON(t, env.app, forms, fiber.MethodPost, "/v1/consents", `{"purpose":"recruitment_pool","email":"r@example.com","emailVerified":true}`); got.status != fiber.StatusCreated {
		t.Fatalf("forms recruitment pool grant: %d %v", got.status, got.body)
	}
	lookup := sendJSON(t, env.app, forms, fiber.MethodPost, "/v1/consents/lookup", `{"purpose":"recruitment_pool","emails":["r@example.com","none@example.com"]}`)
	states, _ := lookup.body["states"].(map[string]any)
	if lookup.status != fiber.StatusOK || states["r@example.com"] != "active" || len(states) != 1 {
		t.Fatalf("lookup %d %v", lookup.status, lookup.body)
	}

	if got := sendJSON(t, env.app, record, fiber.MethodGet, "/v1/consents/audience?purpose=event_invitations", ""); got.status != fiber.StatusForbidden {
		t.Fatalf("a recording service read the audience: %d", got.status)
	}
	audience := sendJSON(t, env.app, skymail, fiber.MethodGet, "/v1/consents/audience?purpose=event_invitations&limit=10", "")
	items, _ := audience.body["items"].([]any)
	if audience.status != fiber.StatusOK || len(items) != 1 || audience.body["next"] != nil {
		t.Fatalf("audience %d %v", audience.status, audience.body)
	}
	entry := items[0].(map[string]any)
	withdrawURL, _ := entry["withdrawUrl"].(string)
	if entry["email"] != "a@example.com" || !strings.HasPrefix(withdrawURL, "https://api.example.test/v1/consents/withdraw?token=") {
		t.Fatalf("entry %v", entry)
	}

	// A scanner opening the link withdraws nothing.
	if page := env.form(t, fiber.MethodGet, pathOf(t, withdrawURL), ""); page.status != fiber.StatusOK || !strings.Contains(page.body, "Onayımı geri al") {
		t.Fatalf("withdraw page %d", page.status)
	}
	if env.count(t, `ended_at IS NOT NULL`) != 0 {
		t.Fatal("opening the withdraw page withdrew")
	}
	// RFC 8058: the mail client POSTs List-Unsubscribe=One-Click to the
	// List-Unsubscribe address; twice is the same.
	for range 2 {
		oneClick := env.form(t, fiber.MethodPost, pathOf(t, withdrawURL), "List-Unsubscribe=One-Click")
		if oneClick.status != fiber.StatusOK || oneClick.header.Get("Location") != "" {
			t.Fatalf("one-click %d %v", oneClick.status, oneClick.header)
		}
	}
	if env.count(t, `ended_via = 'one_click' AND email IS NULL`) != 1 {
		t.Fatal("one-click withdrawal not recorded")
	}
	if page := env.form(t, fiber.MethodPost, consent.WithdrawPath, "token="+url.QueryEscape(tokenIn(t, withdrawURL))); page.status != fiber.StatusOK || !strings.Contains(page.body, "zaten") {
		t.Fatalf("withdraw page again %d %s", page.status, page.body)
	}
	if page := env.form(t, fiber.MethodGet, consent.WithdrawPath+"?token=forged", ""); page.status != fiber.StatusBadRequest {
		t.Fatalf("forged link %d", page.status)
	}
	if got := sendJSON(t, env.app, forms, fiber.MethodPost, "/v1/consents/withdrawals", `{"purpose":"recruitment_pool","email":"r@example.com"}`); got.status != fiber.StatusOK || got.body["withdrawn"] != true {
		t.Fatalf("service withdrawal %d %v", got.status, got.body)
	}
}

func TestPersonManagesTheirOwnConsents(t *testing.T) {
	env := newConsentEnv(t, true)
	token := groupToken(t, env.keys, memberSub)
	got := sendJSON(t, env.app, token, fiber.MethodPost, "/v1/users/me/consents", `{"purpose":"event_invitations"}`)
	if got.status != fiber.StatusCreated || got.body["status"] != "active" {
		t.Fatalf("grant mine %d %v", got.status, got.body)
	}
	if env.count(t, `user_id = '`+memberSub+`' AND email IS NULL AND source = 'self' AND confirmed_via = 'account'`) != 1 {
		t.Fatal("self grant not recorded")
	}
	mine := sendJSON(t, env.app, token, fiber.MethodGet, "/v1/users/me/consents", "")
	items, _ := mine.body["items"].([]any)
	if mine.status != fiber.StatusOK || len(items) != 1 {
		t.Fatalf("mine %d %v", mine.status, mine.body)
	}
	if got := sendJSON(t, env.app, env.serviceToken(t, "place", consent.RoleRecord), fiber.MethodPost, "/v1/users/me/consents", `{"purpose":"event_invitations"}`); got.status != fiber.StatusForbidden {
		t.Fatalf("a service account consented for itself: %d", got.status)
	}
	withdrawn := sendJSON(t, env.app, token, fiber.MethodDelete, "/v1/users/me/consents/event_invitations", "")
	if withdrawn.status != fiber.StatusOK || withdrawn.body["withdrawn"] != float64(1) {
		t.Fatalf("withdraw mine %d %v", withdrawn.status, withdrawn.body)
	}
	if env.count(t, `ended_via = 'self'`) != 1 {
		t.Fatal("self withdrawal not recorded")
	}
}

func TestConsentsOffAnswer503AndGuestApplyStillWorks(t *testing.T) {
	env := newConsentEnv(t, false)
	if got := sendJSON(t, env.app, env.serviceToken(t, "place", consent.RoleRecord), fiber.MethodPost, "/v1/consents", `{"purpose":"event_invitations","email":"a@example.com"}`); got.status != fiber.StatusServiceUnavailable {
		t.Fatalf("record while off %d", got.status)
	}
	if page := env.form(t, fiber.MethodGet, consent.WithdrawPath+"?token=x", ""); page.status != fiber.StatusServiceUnavailable {
		t.Fatalf("page while off %d", page.status)
	}
	got := sendJSON(t, env.app, "", fiber.MethodPost, "/v1/events/"+env.eventID+"/applications/guest",
		`{"firstName":"A","lastName":"B","email":"a@example.com","consents":["event_invitations"]}`)
	if got.status != fiber.StatusCreated || env.count(t, `true`) != 0 {
		t.Fatalf("guest apply while off %d %v", got.status, got.body)
	}
}
