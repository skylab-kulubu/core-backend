package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A profile keeps its picture as the Media, not as an address: the address
// comes from the configured base whenever the profile is read, so moving
// the CDN rewrites nothing stored.
func TestProfilePictureAddressFollowsTheConfiguredBaseHTTP(t *testing.T) {
	t.Parallel()
	users := user.NewMemoryStore()
	mediaStore, blobs := media.NewMemoryStore(), media.NewMemoryBlob()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	id := uuid.MustParse("74747474-7474-7474-7474-747474747474")
	profile := user.Profile{Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper"}
	before := meIdentApp(t, users, id, profile, media.NewService(mediaStore, blobs, az, "https://cdn.example.test"))
	body, ctype := multipartPNG(t, "image", "me.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/users/me/profile-picture", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := before.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	var uploaded user.User
	if err := json.NewDecoder(resp.Body).Decode(&uploaded); err != nil || resp.StatusCode != fiber.StatusOK {
		t.Fatalf("upload %d %v", resp.StatusCode, err)
	}
	stored, err := users.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored.ProfilePictureURL, "images/") {
		t.Fatalf("profile stores %q, want the Media's key", stored.ProfilePictureURL)
	}
	if uploaded.ProfilePictureURL != "https://cdn.example.test/"+stored.ProfilePictureURL {
		t.Fatalf("upload answered %q", uploaded.ProfilePictureURL)
	}

	after := meIdentApp(t, users, id, profile, media.NewService(mediaStore, blobs, az, "https://media.example.org"))
	resp, err = after.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	var me user.User
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.ProfilePictureURL != "https://media.example.org/"+stored.ProfilePictureURL {
		t.Fatalf("after the base moved: %q", me.ProfilePictureURL)
	}
}
