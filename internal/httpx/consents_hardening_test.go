package httpx_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/consent"
)

func (e consentEnv) tickets(t *testing.T, email string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM tickets WHERE guest_email = $1`, email).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Guest apply takes any non-empty address; a consent needs one shaped like
// an address. With the box ticked, such an address is refused before the
// Ticket is written: 400, never a 503 the caller would retry for ever.
func TestGuestApplyRefusesAnAddressAConsentCannotUseBeforeWritingTheTicket(t *testing.T) {
	env := newConsentEnv(t, true)
	for _, email := range []string{"ada@localhost", "ada", "a,b@example.com"} {
		for range 2 {
			got := sendJSON(t, env.app, "", fiber.MethodPost, "/v1/events/"+env.eventID+"/applications/guest",
				`{"firstName":"Ada","lastName":"L","email":"`+email+`","consents":["event_invitations"]}`)
			if got.status != fiber.StatusBadRequest || got.body["code"] != "consent_invalid" {
				t.Fatalf("%q: %d %v", email, got.status, got.body)
			}
		}
		if env.tickets(t, email) != 0 {
			t.Fatalf("%q: a Ticket was written", email)
		}
	}
	if env.count(t, `true`) != 0 {
		t.Fatal("a grant was recorded")
	}
	// Without the box the same address still applies, as before.
	if got := sendJSON(t, env.app, "", fiber.MethodPost, "/v1/events/"+env.eventID+"/applications/guest",
		`{"firstName":"Ada","lastName":"L","email":"ada@localhost"}`); got.status != fiber.StatusCreated {
		t.Fatalf("plain apply %d %v", got.status, got.body)
	}
}

// The consents list names each purpose at most once: repeats of the same
// purpose and text count once; more entries than there are purposes, or one
// purpose with two texts, is 400 before anything is written.
func TestGuestApplyConsentsAreBoundedAndCountedOnce(t *testing.T) {
	env := newConsentEnv(t, true)
	apply := func(email, consents string) jsonResponse {
		return sendJSON(t, env.app, "", fiber.MethodPost, "/v1/events/"+env.eventID+"/applications/guest",
			`{"firstName":"A","lastName":"B","email":"`+email+`","consents":`+consents+`}`)
	}
	var long bytes.Buffer
	long.WriteString(`[`)
	for i := range 20000 {
		if i > 0 {
			long.WriteByte(',')
		}
		long.WriteString(`"event_invitations"`)
	}
	long.WriteString(`]`)
	start := time.Now()
	if got := apply("long@example.com", long.String()); got.status != fiber.StatusBadRequest || got.body["code"] != "consent_invalid" {
		t.Fatalf("20,000 entries %d %v", got.status, got.body)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("20,000 entries took %s", took)
	}
	if got := apply("two@example.com", `["event_invitations",{"purpose":"event_invitations","textVersion":"davet-v9"}]`); got.status != fiber.StatusBadRequest {
		t.Fatalf("two texts %d %v", got.status, got.body)
	}
	if env.tickets(t, "long@example.com") != 0 || env.tickets(t, "two@example.com") != 0 || env.count(t, `true`) != 0 {
		t.Fatal("a refused list wrote something")
	}
	if got := apply("once@example.com", `["event_invitations"]`); got.status != fiber.StatusCreated {
		t.Fatalf("one entry %d %v", got.status, got.body)
	}
	if env.count(t, `email = 'once@example.com'`) != 1 {
		t.Fatal("one entry did not record one grant")
	}
}

// The recruitment pool is not taken yet: every route answers
// purpose_not_enabled and nothing is written.
func TestRecruitmentPoolIsRefusedAsNotEnabled(t *testing.T) {
	env := newConsentEnv(t, true)
	forms := env.serviceToken(t, "forms", consent.RoleRecord)
	skymail := env.serviceToken(t, "skymail", consent.RoleAudienceRead)
	for name, got := range map[string]jsonResponse{
		"record": sendJSON(t, env.app, forms, fiber.MethodPost, "/v1/consents",
			`{"purpose":"recruitment_pool","textVersion":"alim-havuzu-v1","email":"r@example.com"}`),
		"withdrawal": sendJSON(t, env.app, forms, fiber.MethodPost, "/v1/consents/withdrawals", `{"purpose":"recruitment_pool","email":"r@example.com"}`),
		"lookup":     sendJSON(t, env.app, forms, fiber.MethodPost, "/v1/consents/lookup", `{"purpose":"recruitment_pool","emails":["r@example.com"]}`),
		"audience":   sendJSON(t, env.app, skymail, fiber.MethodGet, "/v1/consents/audience?purpose=recruitment_pool", ""),
		"guest apply": sendJSON(t, env.app, "", fiber.MethodPost, "/v1/events/"+env.eventID+"/applications/guest",
			`{"firstName":"A","lastName":"B","email":"r@example.com","consents":["recruitment_pool"]}`),
		"mine": sendJSON(t, env.app, groupToken(t, env.keys, memberSub), fiber.MethodPost, "/v1/users/me/consents", `{"purpose":"recruitment_pool"}`),
	} {
		if got.status != fiber.StatusBadRequest || got.body["code"] != "purpose_not_enabled" {
			t.Errorf("%s: %d %v", name, got.status, got.body)
		}
	}
	if env.count(t, `true`) != 0 || env.tickets(t, "r@example.com") != 0 {
		t.Fatal("a recruitment pool request wrote something")
	}
}

func (e consentEnv) withdrawURL(t *testing.T, email string) string {
	t.Helper()
	record := e.serviceToken(t, "place", consent.RoleRecord)
	if got := sendJSON(t, e.app, record, fiber.MethodPost, "/v1/consents",
		`{"purpose":"event_invitations","email":"`+email+`","emailVerified":true}`); got.status != fiber.StatusCreated {
		t.Fatalf("record %d %v", got.status, got.body)
	}
	audience := sendJSON(t, e.app, e.serviceToken(t, "skymail", consent.RoleAudienceRead), fiber.MethodGet, "/v1/consents/audience?purpose=event_invitations", "")
	items, _ := audience.body["items"].([]any)
	for _, item := range items {
		if entry := item.(map[string]any); entry["email"] == email {
			return entry["withdrawUrl"].(string)
		}
	}
	t.Fatalf("no audience entry for %s: %v", email, audience.body)
	return ""
}

// RFC 8058 one-click as some senders post it: multipart/form-data.
func TestOneClickWithdrawAcceptsAMultipartBody(t *testing.T) {
	env := newConsentEnv(t, true)
	withdrawURL := env.withdrawURL(t, "mp@example.com")
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("List-Unsubscribe", "One-Click")
	_ = w.Close()
	req := httptest.NewRequest(fiber.MethodPost, pathOf(t, withdrawURL), &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := env.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK || resp.Header.Get("Location") != "" || strings.TrimSpace(string(raw)) != "ok" {
		t.Fatalf("multipart one-click %d %q", resp.StatusCode, raw)
	}
	if env.count(t, `ended_via = 'one_click'`) != 1 {
		t.Fatal("multipart one-click did not withdraw")
	}
}

// Pages opened from one address do not use up the withdrawals of that
// address (a mail provider's one-click POSTs share a few addresses).
func TestOneClickWithdrawHasABudgetOfItsOwn(t *testing.T) {
	env := newConsentEnv(t, true)
	withdrawURL := env.withdrawURL(t, "budget@example.com")
	limited := false
	for range 130 {
		if page := env.form(t, fiber.MethodGet, pathOf(t, withdrawURL), ""); page.status == fiber.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("the pages' budget never ran out")
	}
	oneClick := env.form(t, fiber.MethodPost, pathOf(t, withdrawURL), "List-Unsubscribe=One-Click")
	if oneClick.status != fiber.StatusOK {
		t.Fatalf("one-click after the pages' budget ran out: %d", oneClick.status)
	}
	if env.count(t, `ended_via = 'one_click'`) != 1 {
		t.Fatal("one-click did not withdraw")
	}
}
