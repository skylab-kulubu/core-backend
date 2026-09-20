package handlers

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestURLRedirectRejectsTheCommittedDeletionGapWithoutRecordingAHit(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	subjectID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-ffffffffffff")
	if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: "gap@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := users.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.PlatformBlockedAt != nil {
		t.Fatal("test did not stop in the committed pre-projection gap")
	}

	urls := shorturl.NewMemoryStore()
	created := createClubURL(t, urls)
	app := urlAppOnGuard(t, urls, authn.Identity{ID: subjectID}, nil, users.AttributionState)
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusUnauthorized || response.Header.Get(fiber.HeaderLocation) != "" {
		t.Fatalf("status=%d location=%q", response.StatusCode, response.Header.Get(fiber.HeaderLocation))
	}
	if response.Header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("cache-control = %q", response.Header.Get(fiber.HeaderCacheControl))
	}
	hits, err := urls.ListHits(ctx, created.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("committed deletion gap recorded hits = %+v", hits)
	}
	stored, err := urls.Get(ctx, created.ID)
	if err != nil || stored.ClickCount != 0 {
		t.Fatalf("committed deletion gap click count=%d err=%v", stored.ClickCount, err)
	}
}

func TestURLRedirectFailsClosedWhenAttributionDatabaseIsUnavailable(t *testing.T) {
	pool := testpostgres.Start(t)
	users := user.NewPostgresStore(pool)
	pool.Close()

	urls := shorturl.NewMemoryStore()
	created := createClubURL(t, urls)
	subjectID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	app := urlAppOnGuard(t, urls, authn.Identity{ID: subjectID}, nil, users.AttributionState)
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/go/club", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusServiceUnavailable || response.Header.Get(fiber.HeaderLocation) != "" {
		t.Fatalf("status=%d location=%q", response.StatusCode, response.Header.Get(fiber.HeaderLocation))
	}
	if response.Header.Get(fiber.HeaderCacheControl) != "no-store" || response.Header.Get(fiber.HeaderRetryAfter) != "1" {
		t.Fatalf("headers = %v", response.Header)
	}
	hits, err := urls.ListHits(context.Background(), created.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("database failure recorded hits = %+v", hits)
	}
	stored, err := urls.Get(context.Background(), created.ID)
	if err != nil || stored.ClickCount != 0 {
		t.Fatalf("database failure click count=%d err=%v", stored.ClickCount, err)
	}
}
