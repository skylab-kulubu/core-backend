package httpx_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
)

type recordingSelfDeletion struct {
	subject uuid.UUID
}

func (s *recordingSelfDeletion) Begin(_ context.Context, subject uuid.UUID, _ string) (account.SelfDeletionView, error) {
	s.subject = subject
	now := time.Now().UTC()
	return account.SelfDeletionView{
		Receipt: "adr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Status: account.SelfDeletionPending,
		PlatformBlocked: true, RequestedAt: now, UpdatedAt: now, ReceiptExpiresAt: now.Add(time.Hour),
	}, nil
}

func (s *recordingSelfDeletion) Status(context.Context, string) (account.SelfDeletionView, error) {
	return account.SelfDeletionView{}, account.ErrSelfDeletionReceiptNotFound
}

func (s *recordingSelfDeletion) Retry(context.Context, string) (account.SelfDeletionView, error) {
	return account.SelfDeletionView{}, account.ErrSelfDeletionReceiptNotFound
}

// The sudo verifier reaches the self-delete route through the assembled app;
// without one the sudo proof is refused rather than ignored.
func TestSelfDeleteRouteVerifiesSudoProofThroughDeps(t *testing.T) {
	t.Parallel()
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	refuseIDToken := func(string, string) (authn.Identity, error) {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	begin := func(app *fiber.App) int {
		req := httptest.NewRequest(fiber.MethodPost, "/v1/account-deletion-requests/self", nil)
		req.Header.Set(fiber.HeaderAuthorization, "Bearer account-user-token")
		req.Header.Set("X-Sky-Sudo", "fresh-sudo-token")
		req.Header.Set("Idempotency-Key", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	service := &recordingSelfDeletion{}
	app := httpx.New(httpx.Deps{
		SelfDeletion:           service,
		ParseSelfDeleteContext: refuseIDToken,
		ParseSelfDeleteSudo: func(_ context.Context, token, sudoToken string) (authn.Identity, error) {
			if token != "account-user-token" || sudoToken != "fresh-sudo-token" {
				return authn.Identity{}, authn.ErrInvalidToken
			}
			return authn.Identity{ID: subject}, nil
		},
	})
	if status := begin(app); status != fiber.StatusAccepted || service.subject != subject {
		t.Fatalf("status=%d subject=%s", status, service.subject)
	}

	unconfigured := &recordingSelfDeletion{}
	app = httpx.New(httpx.Deps{SelfDeletion: unconfigured, ParseSelfDeleteContext: refuseIDToken})
	if status := begin(app); status != fiber.StatusUnauthorized || unconfigured.subject != uuid.Nil {
		t.Fatalf("status=%d subject=%s", status, unconfigured.subject)
	}
}
