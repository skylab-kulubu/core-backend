package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func ytuLoginProfile() user.Profile {
	return user.Profile{
		Email: "ada@std.yildiz.edu.tr", FirstName: "Ada", LastName: "Lovelace", SchoolEmail: "ada@std.yildiz.edu.tr",
		University: "Yıldız Teknik Üniversitesi", Department: "011",
	}
}

func sendJSON(t *testing.T, app *fiber.App, method, path, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMeFollowsTheYTULoginHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.New()
	app := meIdentApp(t, store, id, ytuLoginProfile(), nil)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, resp)
	if got["ytuLinked"] != true || got["university"] != "Yıldız Teknik Üniversitesi" ||
		got["department"] != "Bilgisayar Mühendisliği" || got["faculty"] != "Bilgisayar ve Bilişim Bilimleri Fakültesi" {
		t.Fatalf("me %v", got)
	}

	for _, body := range []string{`{"department":"Fizik"}`, `{"faculty":""}`, `{"university":"Boğaziçi"}`} {
		resp := sendJSON(t, app, fiber.MethodPatch, "/v1/users/me", body)
		if resp.StatusCode != fiber.StatusConflict {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("patch %s: status %d body %s", body, resp.StatusCode, b)
		}
		if ct := resp.Header.Get(fiber.HeaderContentType); !strings.HasPrefix(ct, "application/problem+json") {
			t.Fatalf("content type %q", ct)
		}
		if p := decodeJSONMap(t, resp); p["code"] != "ytu_managed_field" {
			t.Fatalf("problem %v", p)
		}
	}
	resp = sendJSON(t, app, fiber.MethodPut, "/v1/users/me", `{"firstName":"Ada","lastName":"Lovelace","university":"Yıldız Teknik Üniversitesi","faculty":"","department":""}`)
	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("put clearing department: status %d", resp.StatusCode)
	}

	resp = sendJSON(t, app, fiber.MethodPatch, "/v1/users/me", `{"linkedin":"https://www.linkedin.com/in/ada","department":"Bilgisayar Mühendisliği"}`)
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("linkedin with the stored department: status %d body %s", resp.StatusCode, b)
	}
	if got := decodeJSONMap(t, resp); got["linkedin"] != "https://www.linkedin.com/in/ada" || got["department"] != "Bilgisayar Mühendisliği" {
		t.Fatalf("patched %v", got)
	}
}

func TestMeYTULinkedIsFalseAndEditableWithoutYTUHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.New()
	app := meIdentApp(t, store, id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}, nil)

	resp := sendJSON(t, app, fiber.MethodPatch, "/v1/users/me", `{"university":"Boğaziçi","department":"Fizik"}`)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := decodeJSONMap(t, resp)
	if linked, present := got["ytuLinked"]; !present || linked != false {
		t.Fatalf("ytuLinked must be present and false: %v", got)
	}
	if got["university"] != "Boğaziçi" || got["department"] != "Fizik" {
		t.Fatalf("patched %v", got)
	}
}

func TestAdminCannotOverrideYTUFieldsHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.New()
	dir.PutUser(identity.Person{ID: id, Email: "ada@std.yildiz.edu.tr", FirstName: "Ada", LastName: "Lovelace"})
	if _, _, err := user.NewService(store).Ensure(t.Context(), id, ytuLoginProfile()); err != nil {
		t.Fatal(err)
	}
	admin := identityApp(t, ykIdent(), dir, store)

	resp := patchUserJSON(t, admin, id, `{"department":"Fizik"}`)
	if resp.StatusCode != fiber.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	if p := decodeJSONMap(t, resp); p["code"] != "ytu_managed_field" {
		t.Fatalf("problem %v", p)
	}

	// The superadmin card form sends every field back; unchanged YTÜ values pass.
	resp = patchUserJSON(t, admin, id, `{"firstName":"Ada","lastName":"Lovelace","linkedin":"","university":"Yıldız Teknik Üniversitesi","faculty":"Bilgisayar ve Bilişim Bilimleri Fakültesi","department":"Bilgisayar Mühendisliği","phone":"`+adminOnlyPhone+`"}`)
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	card := decodeJSONMap(t, resp)
	if card["ytuLinked"] != true || card["department"] != "Bilgisayar Mühendisliği" || card["phone"] != adminOnlyPhone {
		t.Fatalf("card %v", card)
	}
}
