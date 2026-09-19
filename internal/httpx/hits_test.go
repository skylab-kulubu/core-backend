package httpx_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

func TestGoHopUsesExistingJWTWithoutKeycloak(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	yk := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	clicker := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	ykTok := keys.Token(t, jwt.MapClaims{
		"sub": yk.String(), "email": "yk@example.com", "groups": []string{"/UYELER/YK"},
	})

	create := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("Authorization", "Bearer "+ykTok)
	createdResp, err := app.Test(create)
	if err != nil {
		t.Fatal(err)
	}
	if createdResp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(createdResp.Body)
		t.Fatalf("create %d body %s", createdResp.StatusCode, b)
	}
	var created shorturl.URL
	if err := json.NewDecoder(createdResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	anon := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	anon.Header.Set("User-Agent", "Safari/18")
	anon.Header.Set("Referer", "https://instagram.com/")
	anon.Header.Set("X-Forwarded-For", "198.51.100.20")
	anonResp, err := app.Test(anon)
	if err != nil {
		t.Fatal(err)
	}
	if anonResp.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("anon redirect %d", anonResp.StatusCode)
	}
	if loc := anonResp.Header.Get("Location"); loc != "https://skylab.com" {
		t.Fatalf("anon location %s", loc)
	}
	if strings.Contains(strings.ToLower(anonResp.Header.Get("Location")), "keycloak") {
		t.Fatalf("sso %s", anonResp.Header.Get("Location"))
	}

	bad := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	bad.Header.Set("Authorization", "Bearer not-a-jwt")
	badResp, err := app.Test(bad)
	if err != nil {
		t.Fatal(err)
	}
	if badResp.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("invalid jwt %d", badResp.StatusCode)
	}
	if loc := badResp.Header.Get("Location"); loc != "https://skylab.com" {
		t.Fatalf("invalid jwt location %s", loc)
	}

	clickTok := keys.Token(t, jwt.MapClaims{
		"sub": clicker.String(), "email": "click@example.com",
	})
	authed := httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil)
	authed.Header.Set("Authorization", "Bearer "+clickTok)
	authedResp, err := app.Test(authed)
	if err != nil {
		t.Fatal(err)
	}
	if authedResp.StatusCode != fiber.StatusMovedPermanently {
		t.Fatalf("authed redirect %d", authedResp.StatusCode)
	}
	if loc := authedResp.Header.Get("Location"); loc != "https://skylab.com" {
		t.Fatalf("authed location %s", loc)
	}

	hitsReq := httptest.NewRequest(fiber.MethodGet, "/v1/urls/"+created.ID.String()+"/hits", nil)
	hitsReq.Header.Set("Authorization", "Bearer "+ykTok)
	hitsResp, err := app.Test(hitsReq)
	if err != nil {
		t.Fatal(err)
	}
	if hitsResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(hitsResp.Body)
		t.Fatalf("hits %d body %s", hitsResp.StatusCode, b)
	}
	var hits []shorturl.Hit
	if err := json.NewDecoder(hitsResp.Body).Decode(&hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits %+v", hits)
	}
	if hits[0].UserID == nil || *hits[0].UserID != clicker {
		t.Fatalf("jwt hop %+v", hits[0])
	}
	if hits[1].UserID != nil || hits[2].UserID != nil {
		t.Fatalf("public hops should stay anonymous %+v", hits)
	}
	if hits[2].IP != "198.51.100.20" || hits[2].UserAgent != "Safari/18" || hits[2].Referer != "https://instagram.com/" {
		t.Fatalf("anon hit %+v", hits[2])
	}
}

func TestSessionQRLogoIsPNGAndNotAShortLinkHit(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	yk := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ykTok := keys.Token(t, jwt.MapClaims{
		"sub": yk.String(), "email": "yk@example.com", "groups": []string{"/UYELER/YK"},
	})
	auth := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+ykTok)
	}

	evReq := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`,
	))
	evReq.Header.Set("Content-Type", "application/json")
	auth(evReq)
	evResp, err := app.Test(evReq)
	if err != nil {
		t.Fatal(err)
	}
	if evResp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(evResp.Body)
		t.Fatalf("event %d %s", evResp.StatusCode, b)
	}
	var ev event.Event
	if err := json.NewDecoder(evResp.Body).Decode(&ev); err != nil {
		t.Fatal(err)
	}

	dayReq := httptest.NewRequest(fiber.MethodPost, "/v1/event-days", strings.NewReader(
		`{"eventId":"`+ev.ID.String()+`","name":"Day 1"}`,
	))
	dayReq.Header.Set("Content-Type", "application/json")
	auth(dayReq)
	dayResp, err := app.Test(dayReq)
	if err != nil {
		t.Fatal(err)
	}
	var day event.Day
	if err := json.NewDecoder(dayResp.Body).Decode(&day); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	sessBody := `{"eventDayId":"` + day.ID.String() + `","title":"Talk","speakerName":"Ada","sessionType":"PRESENTATION","startTime":"` + start.Format(time.RFC3339) + `","endTime":"` + start.Add(time.Hour).Format(time.RFC3339) + `"}`
	sessReq := httptest.NewRequest(fiber.MethodPost, "/v1/sessions", strings.NewReader(sessBody))
	sessReq.Header.Set("Content-Type", "application/json")
	auth(sessReq)
	sessResp, err := app.Test(sessReq)
	if err != nil {
		t.Fatal(err)
	}
	if sessResp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(sessResp.Body)
		t.Fatalf("session %d %s", sessResp.StatusCode, b)
	}
	var sess event.Session
	if err := json.NewDecoder(sessResp.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}

	urlReq := httptest.NewRequest(fiber.MethodPost, "/v1/urls", strings.NewReader(`{"url":"https://skylab.com","alias":"club"}`))
	urlReq.Header.Set("Content-Type", "application/json")
	auth(urlReq)
	urlResp, err := app.Test(urlReq)
	if err != nil {
		t.Fatal(err)
	}
	var created shorturl.URL
	if err := json.NewDecoder(urlResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	qrResp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/sessions/"+sess.ID.String()+"/qr?logo=1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if qrResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(qrResp.Body)
		t.Fatalf("session qr %d %s", qrResp.StatusCode, b)
	}
	if ct := qrResp.Header.Get("Content-Type"); !strings.Contains(ct, "image/png") {
		t.Fatalf("content-type %s", ct)
	}
	body, err := io.ReadAll(qrResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 8 || string(body[:4]) != "\x89PNG" {
		t.Fatalf("not png len=%d", len(body))
	}

	hitsReq := httptest.NewRequest(fiber.MethodGet, "/v1/urls/"+created.ID.String()+"/hits", nil)
	auth(hitsReq)
	hitsResp, err := app.Test(hitsReq)
	if err != nil {
		t.Fatal(err)
	}
	var hits []shorturl.Hit
	if err := json.NewDecoder(hitsResp.Body).Decode(&hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("session qr counted as hit %+v", hits)
	}
}
