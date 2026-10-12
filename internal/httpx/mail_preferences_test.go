package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mailpref"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

type prefEnv struct {
	app    *fiber.App
	keys   *testauth.Bundle
	store  *mailpref.MemoryStore
	svc    *mailpref.Service
	person uuid.UUID
}

// newPrefEnv is the app with a WEBLAB member on a small team tree: WEBLAB
// (public) with FRONT, AIRLAB (public), GIZLI (hidden). enabled false leaves
// the preferences off (no key).
func newPrefEnv(t *testing.T, enabled bool) prefEnv {
	t.Helper()
	keys := testauth.New(t)
	dir := identity.NewMemory()
	public := map[string]string{"public_listing": "true"}
	for _, g := range []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: public},
		{ID: "g-front", Name: "FRONT", Path: "/UYELER/ARGE/WEBLAB/FRONT"},
		{ID: "g-airlab", Name: "AIRLAB", Path: "/UYELER/ARGE/AIRLAB", Attributes: public},
		{ID: "g-gizli", Name: "GIZLI", Path: "/UYELER/ARGE/GIZLI"},
	} {
		dir.PutGroup(g)
	}
	person := uuid.New()
	dir.PutUser(identity.Person{ID: person, Email: "uye@example.test"})
	if err := dir.AddMember(context.Background(), "g-weblab", person); err != nil {
		t.Fatal(err)
	}
	deps := memoryDepsOver(dir, identity.Options{})
	deps.ParseToken = keys.Parse()
	env := prefEnv{keys: keys, store: mailpref.NewMemoryStore(), person: person}
	if enabled {
		env.svc = mailpref.NewService(mailpref.TestConfig(bytes.Repeat([]byte{5}, 32), "https://api.example.test"), env.store, dir, authz.DefaultPrivilegedRoots())
		deps.MailPreferences = env.svc
	}
	env.app = httpx.New(deps)
	return env
}

func (e prefEnv) token(t *testing.T) string {
	t.Helper()
	return groupToken(t, e.keys, e.person.String(), "/UYELER/ARGE/WEBLAB")
}

func (e prefEnv) link(t *testing.T, cats ...mailpref.Category) string {
	t.Helper()
	link, err := e.svc.UnsubscribeURL(e.person, cats)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	return u.RequestURI()
}

func (e prefEnv) send(t *testing.T, req *http.Request) rawAnswer {
	t.Helper()
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return rawAnswer{status: resp.StatusCode, header: resp.Header, body: string(raw)}
}

func (e prefEnv) explicit(t *testing.T) mailpref.Explicit {
	t.Helper()
	rows, err := e.store.List(context.Background(), e.person)
	if err != nil {
		t.Fatal(err)
	}
	return mailpref.ExplicitOf(rows)
}

func publicPage(t *testing.T, a rawAnswer) {
	t.Helper()
	if a.header.Get("Cache-Control") != "no-store" || a.header.Get("Set-Cookie") != "" || a.header.Get("Location") != "" ||
		a.header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers %v", a.header)
	}
}

func TestMailPreferencesOffAnswer503(t *testing.T) {
	e := newPrefEnv(t, false)
	for _, tc := range []struct{ method, body string }{{fiber.MethodGet, ""}, {fiber.MethodPut, `{"items":[{"category":"all","enabled":false}]}`}} {
		got := sendJSON(t, e.app, e.token(t), tc.method, "/v1/users/me/mail-preferences", tc.body)
		if got.status != fiber.StatusServiceUnavailable || got.body["code"] != "mail_preferences_unavailable" {
			t.Fatalf("%s: %d %v", tc.method, got.status, got.body)
		}
	}
	page := e.send(t, httptest.NewRequest(fiber.MethodGet, mailpref.UnsubscribePath+"?t=abc", nil))
	if page.status != fiber.StatusServiceUnavailable {
		t.Fatalf("page %d", page.status)
	}
	publicPage(t, page)
	req := httptest.NewRequest(fiber.MethodPost, mailpref.UnsubscribePath+"?t=abc", strings.NewReader("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := e.send(t, req); got.status != fiber.StatusServiceUnavailable || got.body != "unavailable" || got.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("one-click %d %q", got.status, got.body)
	}
}

func TestMailPreferencesScreenAndChanges(t *testing.T) {
	e := newPrefEnv(t, true)
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me/mail-preferences", nil)
	req.Header.Set("Authorization", "Bearer "+e.token(t))
	got := e.send(t, req)
	if got.status != fiber.StatusOK || got.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("screen %d %s", got.status, got.body)
	}
	var screen mailpref.Screen
	if err := json.Unmarshal([]byte(got.body), &screen); err != nil {
		t.Fatal(err)
	}
	if !screen.Master.Effective || !screen.Club.Duyuru.Effective || len(screen.Teams) != 1 || screen.Teams[0].GroupID != "g-weblab" ||
		len(screen.Teams[0].Children) != 1 || len(screen.Followable) != 1 || screen.Followable[0].GroupID != "g-airlab" {
		t.Fatalf("screen %s", got.body)
	}

	put := sendJSON(t, e.app, e.token(t), fiber.MethodPut, "/v1/users/me/mail-preferences",
		`{"items":[{"category":"team:g-airlab:duyuru","enabled":true},{"category":"team:g-front:haber","enabled":false},{"category":"club:haber","enabled":null}]}`)
	if put.status != fiber.StatusOK {
		t.Fatalf("put %d %v", put.status, put.body)
	}
	if following, _ := put.body["following"].([]any); len(following) != 1 {
		t.Fatalf("following %v", put.body["following"])
	}
	if ex := e.explicit(t); len(ex) != 2 || !ex[mailpref.Team("g-airlab", mailpref.KindAnnouncement)] || ex[mailpref.Team("g-front", mailpref.KindNews)] {
		t.Fatalf("explicit %v", ex)
	}

	for body, code := range map[string]string{
		`{"items":[]}`:                                                 "mail_preference_invalid",
		`{"items":[{"category":"club:haber"}]}`:                        "mail_preference_invalid",
		`{"items":[{"category":"club:haber","enabled":"no"}]}`:         "mail_preference_invalid",
		`{"items":[{"category":"kulup:haber","enabled":false}]}`:       "mail_preference_invalid",
		`{"items":[{"category":"all","enabled":false}],"x":1}`:         "mail_preference_invalid",
		`{"items":[{"category":"team:g-gizli:haber","enabled":true}]}`: "mail_preference_team_unknown",
		`{"items":[{"category":"team:g-arge:haber","enabled":true}]}`:  "mail_preference_team_unknown",
	} {
		if got := sendJSON(t, e.app, e.token(t), fiber.MethodPut, "/v1/users/me/mail-preferences", body); got.status != fiber.StatusBadRequest || got.body["code"] != code {
			t.Fatalf("%s: %d %v", body, got.status, got.body)
		}
	}

	service := e.keys.Token(t, jwt.MapClaims{"sub": uuid.NewString(), "azp": "skymail", "client_id": "skymail"})
	if got := sendJSON(t, e.app, service, fiber.MethodGet, "/v1/users/me/mail-preferences", ""); got.status != fiber.StatusForbidden || got.body["code"] != "mail_preference_person_only" {
		t.Fatalf("service account %d %v", got.status, got.body)
	}
	if got := sendJSON(t, e.app, "", fiber.MethodGet, "/v1/users/me/mail-preferences", ""); got.status != fiber.StatusUnauthorized {
		t.Fatalf("anonymous %d", got.status)
	}
}

// The link: GET names the categories and changes nothing; the one-click
// POST turns off exactly the link's categories and answers 200 without a
// redirect, again on a repeat; the page's buttons reach further.
func TestUnsubscribeLinkRoutes(t *testing.T) {
	e := newPrefEnv(t, true)
	target := e.link(t, mailpref.Team("g-weblab", mailpref.KindAnnouncement), mailpref.Team("g-airlab", mailpref.KindNews))

	page := e.send(t, httptest.NewRequest(fiber.MethodGet, target, nil))
	if page.status != fiber.StatusOK || !strings.Contains(page.body, "WEBLAB · Duyurular") || !strings.Contains(page.body, "AIRLAB · Haberler") ||
		!strings.Contains(page.body, `name="kapsam" value="all"`) || !strings.Contains(page.body, "my.yildizskylab.com") {
		t.Fatalf("page %d %s", page.status, page.body)
	}
	publicPage(t, page)
	if ex := e.explicit(t); len(ex) != 0 {
		t.Fatalf("GET wrote %v", ex)
	}

	for range 2 {
		req := httptest.NewRequest(fiber.MethodPost, target, strings.NewReader("List-Unsubscribe=One-Click"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got := e.send(t, req)
		if got.status != fiber.StatusOK || got.body != "ok" || got.header.Get("Location") != "" || got.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("one-click %d %q %v", got.status, got.body, got.header)
		}
		ex := e.explicit(t)
		if len(ex) != 2 || ex[mailpref.Team("g-weblab", mailpref.KindAnnouncement)] || ex[mailpref.Team("g-airlab", mailpref.KindNews)] {
			t.Fatalf("explicit %v", ex)
		}
	}

	// Multipart, as RFC 8058 allows.
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("List-Unsubscribe", "One-Click")
	_ = w.Close()
	req := httptest.NewRequest(fiber.MethodPost, target, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if got := e.send(t, req); got.status != fiber.StatusOK || got.body != "ok" {
		t.Fatalf("multipart %d %q", got.status, got.body)
	}

	// The page's button posts the token in the form.
	token := strings.TrimPrefix(target, mailpref.UnsubscribePath+"?t=")
	token, _ = url.QueryUnescape(token)
	form := url.Values{"t": {token}, "kapsam": {"sources"}}
	req = httptest.NewRequest(fiber.MethodPost, mailpref.UnsubscribePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got := e.send(t, req)
	if got.status != fiber.StatusOK || !strings.Contains(got.body, "kaydedildi") {
		t.Fatalf("sources %d %s", got.status, got.body)
	}
	publicPage(t, got)
	if ex := e.explicit(t); len(ex) != 4 || ex[mailpref.Team("g-airlab", mailpref.KindAnnouncement)] {
		t.Fatalf("sources %v", ex)
	}
	form.Set("kapsam", "all")
	req = httptest.NewRequest(fiber.MethodPost, mailpref.UnsubscribePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := e.send(t, req); got.status != fiber.StatusOK {
		t.Fatalf("all %d", got.status)
	}
	if ex := e.explicit(t); ex[mailpref.Master()] {
		t.Fatalf("master %v", ex)
	} else if _, ok := ex[mailpref.Master()]; !ok {
		t.Fatalf("no master row %v", ex)
	}
	form.Set("kapsam", "everything")
	req = httptest.NewRequest(fiber.MethodPost, mailpref.UnsubscribePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := e.send(t, req); got.status != fiber.StatusBadRequest {
		t.Fatalf("unknown reach %d", got.status)
	}
}

// A changed token is 400 on the page and on one-click, and writes nothing.
func TestUnsubscribeLinkRefusesAChangedToken(t *testing.T) {
	e := newPrefEnv(t, true)
	target := e.link(t, mailpref.Club(mailpref.KindAnnouncement))
	// A character in the middle of the token: a whole byte of the MAC or
	// the payload, never padding bits.
	at := len(target) - 10
	swap := map[bool]string{true: "B", false: "A"}[target[at] == 'A']
	changed := target[:at] + swap + target[at+1:]
	page := e.send(t, httptest.NewRequest(fiber.MethodGet, changed, nil))
	if page.status != fiber.StatusBadRequest {
		t.Fatalf("page %d", page.status)
	}
	publicPage(t, page)
	req := httptest.NewRequest(fiber.MethodPost, changed, strings.NewReader("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := e.send(t, req); got.status != fiber.StatusBadRequest || got.body != "invalid link" {
		t.Fatalf("one-click %d %q", got.status, got.body)
	}
	if ex := e.explicit(t); len(ex) != 0 {
		t.Fatalf("wrote %v", ex)
	}
}
