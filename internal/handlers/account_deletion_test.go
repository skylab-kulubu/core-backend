package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
