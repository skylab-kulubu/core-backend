package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type handlerSelfDeleteProjector struct {
	store *user.MemoryStore
	now   time.Time
	err   error
}

func (p handlerSelfDeleteProjector) Project(ctx context.Context, request user.DeletionRequest) error {
	if p.err != nil {
		return p.err
	}
	return p.store.MarkDeletionPlatformBlocked(ctx, request.ID, p.now)
}

func selfDeleteApp(t *testing.T, enabled bool, projectionError error) (*fiber.App, *user.MemoryStore) {
	t.Helper()
	store := user.NewMemoryStore()
	now := time.Now().UTC().Truncate(time.Second)
	service, err := account.NewSelfDeletion(store, handlerSelfDeleteProjector{store: store, now: now, err: projectionError}, account.SelfDeletionConfig{
		Enabled: enabled, ReceiptKey: []byte("0123456789abcdef0123456789abcdef"), ReceiptTTL: 90 * 24 * time.Hour,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	handler := NewAccountDeletionHandler(service, func(token, idToken string) (authn.Identity, error) {
		if token != "account-user-token" || idToken != "fresh-reauth-id-token" {
			return authn.Identity{}, authn.ErrInvalidToken
		}
		return authn.Identity{ID: subject}, nil
	}, func(ctx context.Context, token, sudoToken string) (authn.Identity, error) {
		if ctx == nil || token != "account-user-token" {
			return authn.Identity{}, authn.ErrInvalidToken
		}
		switch sudoToken {
		case "fresh-sudo-token":
			return authn.Identity{ID: subject}, nil
		case "sudo-token-realm-cannot-check":
			return authn.Identity{}, fmt.Errorf("%w: introspection status 502", authn.ErrIntrospectionUnavailable)
		default:
			return authn.Identity{}, authn.ErrInvalidToken
		}
	})
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Post("/v1/account-deletion-requests/self", handler.Begin)
	app.Get("/v1/account-deletion-requests/status", handler.Status)
	app.Post("/v1/account-deletion-requests/status/retry", handler.Retry)
	return app, store
}

func TestAccountDeletionHTTPUsesVerifiedSubjectAndReturnsOnlyCoarseReceiptStatus(t *testing.T) {
	t.Parallel()
	app, _ := selfDeleteApp(t, true, nil)
	key := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	req := httptest.NewRequest(fiber.MethodPost, "/v1/account-deletion-requests/self", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer account-user-token")
	req.Header.Set("X-Account-Reauth-Token", "fresh-reauth-id-token")
	req.Header.Set("Idempotency-Key", key)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusAccepted || resp.Header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("status=%d cache=%q", resp.StatusCode, resp.Header.Get(fiber.HeaderCacheControl))
	}
	var started map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	receipt, _ := started["receipt"].(string)
	if !strings.HasPrefix(receipt, "adr_") || started["status"] != "pending" || started["platformBlocked"] != true {
		t.Fatalf("started = %#v", started)
	}
	for _, forbidden := range []string{"subject", "subjectId", "requestId", "sid", "lastErrorCode", "attemptCount"} {
		if _, exists := started[forbidden]; exists {
			t.Fatalf("response exposed %s: %#v", forbidden, started)
		}
	}
	for _, required := range []string{"receipt", "status", "partial", "platformBlocked", "requestedAt", "updatedAt", "completedAt", "receiptExpiresAt"} {
		if _, exists := started[required]; !exists {
			t.Fatalf("start response omitted %s: %#v", required, started)
		}
	}
	if len(started) != 8 {
		t.Fatalf("start response has unknown fields: %#v", started)
	}

	statusReq := httptest.NewRequest(fiber.MethodGet, "/v1/account-deletion-requests/status", nil)
	statusReq.Header.Set(fiber.HeaderAuthorization, "DeletionReceipt "+receipt)
	statusResp, err := app.Test(statusReq)
	if err != nil {
		t.Fatal(err)
	}
	if statusResp.StatusCode != fiber.StatusOK || statusResp.Header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("status read=%d cache=%q", statusResp.StatusCode, statusResp.Header.Get(fiber.HeaderCacheControl))
	}
	var status map[string]any
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if _, exposed := status["receipt"]; exposed {
		t.Fatalf("status reflected capability: %#v", status)
	}
	if status["status"] != "pending" || status["partial"] != false {
		t.Fatalf("status = %#v", status)
	}
	for _, required := range []string{"status", "partial", "platformBlocked", "requestedAt", "updatedAt", "completedAt", "receiptExpiresAt"} {
		if _, exists := status[required]; !exists {
			t.Fatalf("status response omitted %s: %#v", required, status)
		}
	}
	if len(status) != 7 {
		t.Fatalf("status response has unknown fields: %#v", status)
	}
}

func TestAccountDeletionHTTPFailsClosedWithoutFreshAuthKeyOrProjection(t *testing.T) {
	t.Parallel()
	key := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	tests := []struct {
		name       string
		enabled    bool
		projectErr error
		auth       string
		key        string
		noReauth   bool
		want       int
		code       string
	}{
		{name: "missing auth", enabled: true, key: key, want: 401, code: "invalid_end_user_token"},
		{name: "wrong scheme", enabled: true, auth: "DeletionReceipt nope", key: key, want: 401, code: "invalid_end_user_token"},
		{name: "missing reauth proof", enabled: true, auth: "Bearer account-user-token", key: key, noReauth: true, want: 401, code: "invalid_end_user_token"},
		{name: "missing key", enabled: true, auth: "Bearer account-user-token", want: 400, code: "invalid_idempotency_key"},
		{name: "projection unavailable", enabled: true, projectErr: errors.New("redis down"), auth: "Bearer account-user-token", key: key, want: 503, code: "account_deletion_unavailable"},
		{name: "disabled", enabled: false, auth: "Bearer account-user-token", key: key, want: 503, code: "account_deletion_unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := selfDeleteApp(t, tc.enabled, tc.projectErr)
			req := httptest.NewRequest(fiber.MethodPost, "/v1/account-deletion-requests/self", nil)
			if tc.auth != "" {
				req.Header.Set(fiber.HeaderAuthorization, tc.auth)
				if strings.HasPrefix(tc.auth, "Bearer ") && !tc.noReauth {
					req.Header.Set("X-Account-Reauth-Token", "fresh-reauth-id-token")
				}
			}
			if tc.key != "" {
				req.Header.Set("Idempotency-Key", tc.key)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want || resp.Header.Get(fiber.HeaderCacheControl) != "no-store" {
				t.Fatalf("status=%d cache=%q", resp.StatusCode, resp.Header.Get(fiber.HeaderCacheControl))
			}
			var problem map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
				t.Fatal(err)
			}
			if problem["code"] != tc.code {
				t.Fatalf("problem = %#v", problem)
			}
		})
	}
}

// Account Center moves from the fresh ID token to its Sudo mode token while
// both services ship independently, so either proof is accepted. When both
// arrive the sudo token decides alone: a refused sudo proof is not rescued by
// the ID token, and the realm being unreachable is "try again", not a refusal.
func TestAccountDeletionHTTPAcceptsSudoProofAndPrefersItOverIDToken(t *testing.T) {
	t.Parallel()
	key := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	tests := []struct {
		name    string
		sudo    string
		reauth  string
		want    int
		code    string
		retry   bool
		invalid bool
	}{
		{name: "sudo proof alone", sudo: "fresh-sudo-token", want: 202},
		{name: "id token alone", reauth: "fresh-reauth-id-token", want: 202},
		{name: "both, sudo decides", sudo: "fresh-sudo-token", reauth: "stale-reauth-id-token", want: 202},
		{name: "both, refused sudo is not rescued", sudo: "stale-sudo-token", reauth: "fresh-reauth-id-token", want: 401, code: "invalid_end_user_token", invalid: true},
		{name: "refused sudo proof", sudo: "stale-sudo-token", want: 401, code: "invalid_end_user_token", invalid: true},
		{name: "malformed sudo header", sudo: "fresh-sudo-token, fresh-sudo-token", reauth: "fresh-reauth-id-token", want: 401, code: "invalid_end_user_token", invalid: true},
		{name: "two sudo tokens", sudo: "fresh-sudo-token fresh-sudo-token", want: 401, code: "invalid_end_user_token", invalid: true},
		{name: "realm cannot check the proof", sudo: "sudo-token-realm-cannot-check", reauth: "fresh-reauth-id-token", want: 503, code: "account_deletion_unavailable", retry: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, _ := selfDeleteApp(t, true, nil)
			req := httptest.NewRequest(fiber.MethodPost, "/v1/account-deletion-requests/self", nil)
			req.Header.Set(fiber.HeaderAuthorization, "Bearer account-user-token")
			req.Header.Set("Idempotency-Key", key)
			if tc.sudo != "" {
				req.Header["X-Sky-Sudo"] = []string{tc.sudo}
			}
			if tc.reauth != "" {
				req.Header.Set("X-Account-Reauth-Token", tc.reauth)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want || resp.Header.Get(fiber.HeaderCacheControl) != "no-store" {
				t.Fatalf("status=%d cache=%q", resp.StatusCode, resp.Header.Get(fiber.HeaderCacheControl))
			}
			if got := resp.Header.Get(fiber.HeaderWWWAuthenticate) != ""; got != tc.invalid {
				t.Fatalf("WWW-Authenticate=%q", resp.Header.Get(fiber.HeaderWWWAuthenticate))
			}
			if got := resp.Header.Get(fiber.HeaderRetryAfter) != ""; got != tc.retry {
				t.Fatalf("Retry-After=%q", resp.Header.Get(fiber.HeaderRetryAfter))
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if tc.code == "" {
				if receipt, _ := body["receipt"].(string); !strings.HasPrefix(receipt, "adr_") {
					t.Fatalf("started = %#v", body)
				}
				return
			}
			if body["code"] != tc.code {
				t.Fatalf("problem = %#v", body)
			}
			for _, secret := range []string{tc.sudo, tc.reauth} {
				if secret != "" && strings.Contains(fmt.Sprint(body), secret) {
					t.Fatalf("problem echoed a proof: %#v", body)
				}
			}
		})
	}
}

// A Core without introspection configured refuses the sudo proof rather than
// quietly falling back to the other header.
func TestAccountDeletionHTTPRefusesSudoProofWithoutVerifier(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	service, err := account.NewSelfDeletion(store, handlerSelfDeleteProjector{store: store, now: time.Now().UTC()}, account.SelfDeletionConfig{
		Enabled: true, ReceiptKey: []byte("0123456789abcdef0123456789abcdef"), ReceiptTTL: 90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewAccountDeletionHandler(service, func(string, string) (authn.Identity, error) {
		return authn.Identity{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111")}, nil
	}, nil)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Post("/v1/account-deletion-requests/self", handler.Begin)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/account-deletion-requests/self", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer account-user-token")
	req.Header.Set("X-Sky-Sudo", "fresh-sudo-token")
	req.Header.Set("X-Account-Reauth-Token", "fresh-reauth-id-token")
	req.Header.Set("Idempotency-Key", base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestAccountDeletionReceiptFailuresAreGenericAndNeverEchoCapability(t *testing.T) {
	t.Parallel()
	app, _ := selfDeleteApp(t, true, nil)
	for _, method := range []string{fiber.MethodGet, fiber.MethodPost} {
		path := "/v1/account-deletion-requests/status"
		if method == fiber.MethodPost {
			path += "/retry"
		}
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(fiber.HeaderAuthorization, "DeletionReceipt adr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusNotFound {
			t.Fatalf("%s status = %d", method, resp.StatusCode)
		}
		var problem map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
			t.Fatal(err)
		}
		if problem["code"] != "account_deletion_receipt_not_found" || strings.Contains(problem["detail"].(string), "adr_") {
			t.Fatalf("problem = %#v", problem)
		}
	}
}
