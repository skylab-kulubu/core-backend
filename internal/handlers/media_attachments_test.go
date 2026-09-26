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
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// serviceAttachApp serves the service attach API to a product's service
// account holding media:attach, over store.
func serviceAttachApp(t *testing.T, product authz.Product, store *media.MemoryStore) *fiber.App {
	t.Helper()
	ident := authn.Identity{ID: uuid.New(), ServiceAccount: true, Product: product, Roles: []string{"media:attach"}}
	return mediaApp(t, ident, store, media.NewMemoryBlob())
}

func sendAttachment(t *testing.T, app *fiber.App, method, path, body string) uploadResponse {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
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
	got := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("status %d body %s: %v", resp.StatusCode, raw, err)
		}
	}
	return uploadResponse{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: got}
}

func postAttachment(t *testing.T, app *fiber.App, mediaID, body string) uploadResponse {
	t.Helper()
	return sendAttachment(t, app, fiber.MethodPost, "/v1/media/"+mediaID+"/attachments", body)
}

func storedMedia(t *testing.T, store *media.MemoryStore, purpose string, uploader uuid.UUID) media.Media {
	t.Helper()
	created, err := store.Create(t.Context(), media.Media{
		Name: purpose, Type: "application/pdf", Kind: media.KindFile, Key: "files/" + uuid.NewString(),
		UploadedBy: uploader, Purpose: purpose,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func attachmentJSON(service, ownerType, ownerID, role, onBehalfOf string) string {
	return `{"owner":{"service":"` + service + `","type":"` + ownerType + `","id":"` + ownerID + `"},"role":"` + role +
		`","onBehalfOf":"` + onBehalfOf + `"}`
}

// The service attach API refuses a link for its Media with the link codes
// core's own links use. A Media the product may not link answers exactly
// like a Media that does not exist.
func TestServiceAttachRefusalsHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	app := serviceAttachApp(t, authz.ProductCMS, store)
	editor := uuid.New()
	bylaws := storedMedia(t, store, "cms_file", editor)
	answer := storedMedia(t, store, "answer_file", editor)
	page := "skylab-site:hakkimizda"

	resp := postAttachment(t, app, bylaws.ID.String(), attachmentJSON("cms", "page", page, "image", editor.String()))
	requireProblem(t, resp, fiber.StatusUnprocessableEntity, "media_purpose_mismatch")
	if resp.body["mediaId"] != bylaws.ID.String() || resp.body["role"] != "image" || resp.body["purpose"] != "cms_file" {
		t.Fatalf("problem %v", resp.body)
	}

	missingID := uuid.NewString()
	notOurs := postAttachment(t, app, answer.ID.String(), attachmentJSON("cms", "page", page, "file", editor.String()))
	missing := postAttachment(t, app, missingID, attachmentJSON("cms", "page", page, "file", editor.String()))
	for name, resp := range map[string]uploadResponse{"another product's Media": notOurs, "missing Media": missing} {
		requireProblem(t, resp, fiber.StatusUnprocessableEntity, "media_not_linkable")
		delete(resp.body, "mediaId")
		delete(resp.body, "instance")
		if len(resp.body) != 6 || resp.body["role"] != "file" || resp.body["purpose"] != nil {
			t.Errorf("%s: problem %v", name, resp.body)
		}
	}
	if notOurs.body["detail"] != missing.body["detail"] {
		t.Fatalf("another product's Media and a missing one answer differently: %v / %v", notOurs.body, missing.body)
	}

	resp = postAttachment(t, app, bylaws.ID.String(), attachmentJSON("cms", "page", page, "cover", editor.String()))
	requireProblem(t, resp, fiber.StatusBadRequest, "media_role_unknown")
	if resp.body["role"] != "cover" {
		t.Fatalf("problem %v", resp.body)
	}

	for name, body := range map[string]string{
		"owner id with a space": attachmentJSON("cms", "page", "skylab-site:hakkımızda sayfası", "file", editor.String()),
		"owner type missing":    attachmentJSON("cms", "", page, "file", editor.String()),
		"no acting person":      attachmentJSON("cms", "page", page, "file", ""),
		"acting person not id":  attachmentJSON("cms", "page", page, "file", "editor"),
		"not JSON":              `owner=cms`,
	} {
		resp = postAttachment(t, app, bylaws.ID.String(), body)
		if resp.status != fiber.StatusBadRequest || resp.body["code"] != nil {
			t.Errorf("%s: status %d body %v", name, resp.status, resp.body)
		}
	}

	resp = postAttachment(t, app, bylaws.ID.String(), attachmentJSON("cms", "page", page, "file", editor.String()))
	if owner, _ := resp.body["owner"].(map[string]any); resp.status != fiber.StatusCreated || owner["id"] != page {
		t.Fatalf("a CMS page as clientId:slug: status %d body %v", resp.status, resp.body)
	}
}

// A person is refused before anything in the request is read: whatever they
// send, the answer is 403.
func TestServiceAttachRefusesAPersonBeforeReadingTheRequestHTTP(t *testing.T) {
	t.Parallel()
	person := authn.Identity{ID: uuid.New(), Roles: []string{"media:attach"}}
	app := mediaApp(t, person, media.NewMemoryStore(), media.NewMemoryBlob())

	for name, call := range map[string][3]string{
		"attach, not JSON":         {fiber.MethodPost, "/v1/media/" + uuid.NewString() + "/attachments", `owner=cms`},
		"attach, bad Media id":     {fiber.MethodPost, "/v1/media/logo/attachments", attachmentJSON("cms", "page", "home", "image", uuid.NewString())},
		"detach, bad ids":          {fiber.MethodDelete, "/v1/media/logo/attachments/first", ""},
		"attach, valid everything": {fiber.MethodPost, "/v1/media/" + uuid.NewString() + "/attachments", attachmentJSON("cms", "page", "home", "image", uuid.NewString())},
	} {
		resp := sendAttachment(t, app, call[0], call[1], call[2])
		if resp.status != fiber.StatusForbidden || resp.body["code"] != "media_attach_forbidden" {
			t.Errorf("%s: status %d body %v", name, resp.status, resp.body)
		}
	}
}
