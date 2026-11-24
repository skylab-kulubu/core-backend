package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
)

const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"

func shortLinkMiss(t *testing.T, app *fiber.App, path, accept string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set(fiber.HeaderAccept, accept)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// appWithDisabledClub serves a store where "club" was a link that is now
// disabled and "kulup" its retired spelling.
func appWithDisabledClub(t *testing.T) *fiber.App {
	t.Helper()
	store := shorturl.NewMemoryStore()
	created := createClubURL(t, store)
	renamed := created
	renamed.Alias = "kulup2"
	if _, err := store.Update(context.Background(), renamed); err != nil {
		t.Fatal(err)
	}
	renamed.Alias = "club"
	if _, err := store.Update(context.Background(), renamed); err != nil {
		t.Fatal(err)
	}
	if err := store.Disable(context.Background(), created.ID, nil); err != nil {
		t.Fatal(err)
	}
	return urlAppOn(t, store, authn.Identity{}, nil)
}

func TestShortLinkMissKeepsTheProblemJSONForAPIClients(t *testing.T) {
	app := appWithDisabledClub(t)
	for _, path := range []string{"/v1/go/missing", "/v1/go/club", "/v1/go/kulup2", "/v1/go/missing/ig"} {
		for _, accept := range []string{"", "application/json", "*/*", "application/json, text/html", "application/problem+json"} {
			resp, body := shortLinkMiss(t, app, path, accept)
			if resp.StatusCode != fiber.StatusNotFound {
				t.Fatalf("%s accept=%q: status %d", path, accept, resp.StatusCode)
			}
			if ct := resp.Header.Get(fiber.HeaderContentType); ct != "application/problem+json" {
				t.Fatalf("%s accept=%q: content-type %q", path, accept, ct)
			}
			var problem map[string]any
			if err := json.Unmarshal([]byte(body), &problem); err != nil {
				t.Fatalf("%s accept=%q: %v %s", path, accept, err, body)
			}
			if problem["title"] != "Not Found" || problem["detail"] != "Not Found" || problem["status"] != float64(404) {
				t.Fatalf("%s accept=%q: body %s", path, accept, body)
			}
			if resp.Header.Get(fiber.HeaderVary) != fiber.HeaderAccept {
				t.Fatalf("%s accept=%q: vary %q", path, accept, resp.Header.Get(fiber.HeaderVary))
			}
		}
	}
}

func TestShortLinkMissShowsABrowserAPage(t *testing.T) {
	app := appWithDisabledClub(t)
	for _, test := range []struct {
		path   string
		status int
		title  string
	}{
		{path: "/v1/go/missing", status: fiber.StatusNotFound, title: "Bu sayfa yok"},
		{path: "/v1/go/missing/ig", status: fiber.StatusNotFound, title: "Bu sayfa yok"},
		{path: "/v1/go/club", status: fiber.StatusGone, title: "Bu bağlantının süresi doldu"},
		{path: "/v1/go/kulup2", status: fiber.StatusGone, title: "Bu bağlantının süresi doldu"},
		{path: "/v1/go/club/wa", status: fiber.StatusGone, title: "Bu bağlantının süresi doldu"},
	} {
		for _, accept := range []string{browserAccept, "text/html"} {
			resp, body := shortLinkMiss(t, app, test.path, accept)
			if resp.StatusCode != test.status {
				t.Fatalf("%s: status %d", test.path, resp.StatusCode)
			}
			if ct := resp.Header.Get(fiber.HeaderContentType); ct != "text/html; charset=utf-8" {
				t.Fatalf("%s: content-type %q", test.path, ct)
			}
			if cc := resp.Header.Get(fiber.HeaderCacheControl); cc != "no-store" {
				t.Fatalf("%s: cache-control %q", test.path, cc)
			}
			if resp.Header.Get(fiber.HeaderVary) != fiber.HeaderAccept || resp.Header.Get(fiber.HeaderLocation) != "" {
				t.Fatalf("%s: headers %v", test.path, resp.Header)
			}
			for _, want := range []string{`<html lang="tr">`, "<h1>" + test.title + "</h1>", `href="https://yildizskylab.com"`, "prefers-reduced-motion: reduce", "prefers-color-scheme: light", `aria-hidden="true"`} {
				if !strings.Contains(body, want) {
					t.Fatalf("%s: page lacks %q", test.path, want)
				}
			}
			if strings.Contains(body, "missing") || strings.Contains(body, "kulup2") {
				t.Fatalf("%s: page reflects the request", test.path)
			}
		}
	}
}

// The page loads nothing from anywhere and its only style is the <style>
// element the policy names by hash.
func TestShortLinkMissPageIsSelfContained(t *testing.T) {
	app := urlApp(t, authn.Identity{})
	resp, body := shortLinkMiss(t, app, "/v1/go/missing", browserAccept)
	csp := resp.Header.Get(fiber.HeaderContentSecurityPolicy)
	if !strings.HasPrefix(csp, "default-src 'none'; ") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("csp %q", csp)
	}
	if resp.Header.Get(fiber.HeaderXContentTypeOptions) != "nosniff" {
		t.Fatalf("nosniff missing: %v", resp.Header)
	}
	styles := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindAllStringSubmatch(body, -1)
	if len(styles) != 1 {
		t.Fatalf("style elements = %d", len(styles))
	}
	sum := sha256.Sum256([]byte(styles[0][1]))
	if want := "style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, want) {
		t.Fatalf("csp %q does not name the style hash %q", csp, want)
	}
	// Safari does not resolve var() in an SVG presentation attribute; the
	// colours are classes in the one <style>.
	for _, banned := range []string{"<script", " style=", "<link", "<img", "@import", "url(", "src=", `="var(`} {
		if strings.Contains(body, banned) {
			t.Fatalf("page holds %q", banned)
		}
	}
	if n := strings.Count(body, "http"); n != 1 {
		t.Fatalf("page names %d addresses, want only the home link", n)
	}
}

func TestShortLinkHopStillRedirectsABrowser(t *testing.T) {
	store := shorturl.NewMemoryStore()
	createClubURL(t, store)
	app := urlAppOn(t, store, authn.Identity{}, nil)
	resp, _ := shortLinkMiss(t, app, "/v1/go/club", browserAccept)
	if resp.StatusCode != fiber.StatusMovedPermanently || resp.Header.Get(fiber.HeaderLocation) == "" {
		t.Fatalf("status %d location %q", resp.StatusCode, resp.Header.Get(fiber.HeaderLocation))
	}
}
