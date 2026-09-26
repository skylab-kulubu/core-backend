package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// serviceAttachApp serves the service attach API to the service account of
// a Keycloak client holding media:attach, over store.
func serviceAttachApp(t *testing.T, client string, store *media.MemoryStore) *fiber.App {
	t.Helper()
	ident := authn.Identity{ID: uuid.New(), Client: client, ServiceAccount: true, Roles: []string{"media:attach"}}
	return mediaApp(t, ident, store, media.NewMemoryBlob())
}

func postAttachment(t *testing.T, app *fiber.App, mediaID, body string) uploadResponse {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media/"+mediaID+"/attachments", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("status %d body %s: %v", resp.StatusCode, raw, err)
	}
	return uploadResponse{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: got}
}

func storedMedia(t *testing.T, store *media.MemoryStore, purpose string) media.Media {
	t.Helper()
	created, err := store.Create(t.Context(), media.Media{
		Name: purpose, Type: "application/pdf", Kind: media.KindFile, Key: "files/" + uuid.NewString(),
		UploadedBy: uuid.New(), Purpose: purpose,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func attachmentJSON(service, ownerType, ownerID, role string) string {
	return `{"owner":{"service":"` + service + `","type":"` + ownerType + `","id":"` + ownerID + `"},"role":"` + role + `"}`
}

// The service attach API refuses a link for its Media with the link codes
// core's own links use, plus the product rules.
func TestServiceAttachRefusalsHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	app := serviceAttachApp(t, "skycms", store)
	bylaws := storedMedia(t, store, "cms_file")
	answer := storedMedia(t, store, "answer_file")
	page := uuid.NewString()

	resp := postAttachment(t, app, bylaws.ID.String(), attachmentJSON("cms", "page", page, "image"))
	requireProblem(t, resp, fiber.StatusUnprocessableEntity, "media_purpose_mismatch")
	if resp.body["mediaId"] != bylaws.ID.String() || resp.body["role"] != "image" || resp.body["purpose"] != "cms_file" {
		t.Fatalf("problem %v", resp.body)
	}

	resp = postAttachment(t, app, answer.ID.String(), attachmentJSON("cms", "page", page, "file"))
	requireProblem(t, resp, fiber.StatusForbidden, "media_product_mismatch")
	if resp.body["mediaId"] != answer.ID.String() || resp.body["role"] != "file" || resp.body["purpose"] != nil {
		t.Fatalf("problem %v", resp.body)
	}

	resp = postAttachment(t, app, uuid.NewString(), attachmentJSON("cms", "page", page, "file"))
	requireProblem(t, resp, fiber.StatusUnprocessableEntity, "media_not_linkable")

	resp = postAttachment(t, app, bylaws.ID.String(), attachmentJSON("cms", "page", page, "cover"))
	requireProblem(t, resp, fiber.StatusBadRequest, "media_role_unknown")
	if resp.body["role"] != "cover" {
		t.Fatalf("problem %v", resp.body)
	}

	for name, body := range map[string]string{
		"owner id not a UUID": attachmentJSON("cms", "page", "home", "file"),
		"owner type missing":  attachmentJSON("cms", "", page, "file"),
		"not JSON":            `owner=cms`,
	} {
		resp = postAttachment(t, app, bylaws.ID.String(), body)
		if resp.status != fiber.StatusBadRequest || resp.body["code"] != nil {
			t.Errorf("%s: status %d body %v", name, resp.status, resp.body)
		}
	}
}
