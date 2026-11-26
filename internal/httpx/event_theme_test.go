package httpx_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/handlers"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// The themes' catalogue answers anyone, signed in or not, the default
// first; an Event takes a theme and a ticket colour by PATCH, and refuses
// an unknown theme or a colour that is none with its own problem code
// (docs/event-theme.md).
func TestEventThemes(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/event-themes", nil))
	if err != nil {
		t.Fatal(err)
	}
	var themes []handlers.EventThemeView
	if err := json.NewDecoder(resp.Body).Decode(&themes); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK || resp.Header.Get("Cache-Control") != "public, max-age=300" || len(themes) != 2 ||
		themes[0].Key != "kurumsal" || !themes[0].Default || themes[1].Key != "artlab" || themes[1].Default ||
		themes[1].Name != "ARTLAB" || themes[1].Preview.Paper != "#0A1222" || themes[1].Preview.Accent != "#FF8A1F" {
		t.Fatalf("%d %+v", resp.StatusCode, themes)
	}

	token := "Bearer " + keys.Token(t, jwt.MapClaims{
		"sub": "11111111-1111-1111-1111-111111111111", "email": "yk@example.com", "groups": []string{"/UYELER/YK"},
	})
	send := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", token)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	status, created := send(fiber.MethodPost, "/v1/events", `{"name":"ARTLAB","location":"YTÜ","ownerTeam":"ARTLAB","theme":"artlab"}`)
	if status != fiber.StatusCreated || created["theme"] != "artlab" || created["ticketAccentColor"] != nil {
		t.Fatalf("create %d %v", status, created)
	}
	id, _ := created["id"].(string)
	for body, code := range map[string]string{
		`{"theme":"neon"}`:              "event_theme_unknown",
		`{"ticketAccentColor":"#12"}`:   "event_ticket_accent_color_invalid",
		`{"ticketAccentColor":"mavi"}`:  "event_ticket_accent_color_invalid",
		`{"theme":"ARTLAB","name":"x"}`: "event_theme_unknown",
	} {
		status, problem := send(fiber.MethodPatch, "/v1/events/"+id, body)
		if status != fiber.StatusBadRequest || problem["code"] != code {
			t.Errorf("%s: %d %v", body, status, problem)
		}
	}
	status, patched := send(fiber.MethodPatch, "/v1/events/"+id, `{"ticketAccentColor":"#3aa0ff"}`)
	if status != fiber.StatusOK || patched["theme"] != "artlab" || patched["ticketAccentColor"] != "#3AA0FF" {
		t.Fatalf("patch %d %v", status, patched)
	}
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+id, nil))
	if err != nil {
		t.Fatal(err)
	}
	var read event.Event
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil || read.ThemeKey() != "artlab" {
		t.Fatalf("public read %v %v", read.Theme, err)
	}
}
