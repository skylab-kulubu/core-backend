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

type uploadResponse struct {
	status      int
	contentType string
	body        map[string]any
}

// postMedia uploads data as the multipart "file" field, with a "purpose"
// field when purpose is not empty.
func postMedia(t *testing.T, app *fiber.App, purpose, filename string, data []byte) uploadResponse {
	t.Helper()
	var values map[string]string
	if purpose != "" {
		values = map[string]string{"purpose": purpose}
	}
	body, contentType := multipartFile(t, values, "file", filename, data)
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", body)
	req.Header.Set("Content-Type", contentType)
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

func TestMediaUploadWithPurposeHTTP(t *testing.T) {
	t.Parallel()
	organizer := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/YK"}}
	app := mediaApp(t, organizer, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "event_cover", "cover.png", pngDotHTTP())
	if resp.status != fiber.StatusCreated || resp.body["purpose"] != "event_cover" {
		t.Fatalf("status %d body %v", resp.status, resp.body)
	}
}

// requireProblem checks a refused upload: problem+json with the status and
// the stable code.
func requireProblem(t *testing.T, resp uploadResponse, wantStatus int, wantCode string) {
	t.Helper()
	if resp.status != wantStatus || !strings.HasPrefix(resp.contentType, "application/problem+json") ||
		resp.body["code"] != wantCode || resp.body["status"] != float64(wantStatus) {
		t.Fatalf("status %d %s body %v; want %d %s", resp.status, resp.contentType, resp.body, wantStatus, wantCode)
	}
}

func TestMediaUploadForUnknownPurposeHTTP(t *testing.T) {
	t.Parallel()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "banner", "dot.png", pngDotHTTP())
	requireProblem(t, resp, fiber.StatusBadRequest, "purpose_unknown")
	if resp.body["purpose"] != "banner" {
		t.Fatalf("problem %v", resp.body)
	}
}

func TestMediaUploadOfTheWrongTypeHTTP(t *testing.T) {
	t.Parallel()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "profile_picture", "me.png", []byte("%PDF-1.7\n"))
	requireProblem(t, resp, fiber.StatusUnsupportedMediaType, "media_type_not_allowed")
	allowed, _ := resp.body["allowedTypes"].([]any)
	if resp.body["purpose"] != "profile_picture" || len(allowed) != 4 || allowed[0] != "image/jpeg" {
		t.Fatalf("problem %v", resp.body)
	}
}

func TestMediaUploadAboveThePurposeMaximumHTTP(t *testing.T) {
	t.Parallel()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, media.NewMemoryStore(), media.NewMemoryBlob())
	picture := make([]byte, 5<<20+1)
	copy(picture, pngDotHTTP())

	resp := postMedia(t, app, "profile_picture", "me.png", picture)
	requireProblem(t, resp, fiber.StatusRequestEntityTooLarge, "media_too_large")
	if resp.body["purpose"] != "profile_picture" || resp.body["maxBytes"] != float64(5<<20) {
		t.Fatalf("problem %v", resp.body)
	}
}

func TestMediaUploadBySomeoneThePurposeDoesNotAllowHTTP(t *testing.T) {
	t.Parallel()
	member := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app := mediaApp(t, member, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "event_cover", "cover.png", pngDotHTTP())
	requireProblem(t, resp, fiber.StatusForbidden, "purpose_forbidden")
	if resp.body["purpose"] != "event_cover" {
		t.Fatalf("problem %v", resp.body)
	}
}

func TestMediaUploadForAPrivatePurposeIsRefusedHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, store, media.NewMemoryBlob())

	resp := postMedia(t, app, "answer_file", "cv.pdf", []byte("%PDF-1.7\n"))
	requireProblem(t, resp, fiber.StatusUnprocessableEntity, "private_media_disabled")
	if stored, _ := store.List(t.Context()); len(stored) != 0 || resp.body["purpose"] != "answer_file" {
		t.Fatalf("problem %v, stored %d", resp.body, len(stored))
	}
}

func TestMediaUploadForAPurposeNothingCanAttachYetHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, store, media.NewMemoryBlob())

	resp := postMedia(t, app, "cms_file", "bylaws.pdf", []byte("%PDF-1.7\n"))
	requireProblem(t, resp, fiber.StatusUnprocessableEntity, "purpose_not_available")
	if stored, _ := store.List(t.Context()); len(stored) != 0 || resp.body["purpose"] != "cms_file" ||
		!strings.Contains(resp.body["detail"].(string), "ticket 03") {
		t.Fatalf("problem %v, stored %d", resp.body, len(stored))
	}
}

func TestMediaUploadForADirectUploadPurposeHTTP(t *testing.T) {
	t.Parallel()
	organizer := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/YK"}}
	app := mediaApp(t, organizer, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "video", "talk.mp4", []byte("\x00\x00\x00\x18ftypmp42"))
	requireProblem(t, resp, fiber.StatusBadRequest, "purpose_requires_direct_upload")
	if resp.body["purpose"] != "video" {
		t.Fatalf("problem %v", resp.body)
	}
}

// Media uploaded without a purpose keep the rules they had before Media
// purpose until Skyforms and CMS send purposes (ADR-0052): any named file up
// to 20 MiB, SVG kept, a PDF only under a .pdf name, and no purpose codes.
func TestMediaUploadWithoutPurposeKeepsTheLegacyRulesHTTP(t *testing.T) {
	t.Parallel()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, media.NewMemoryStore(), media.NewMemoryBlob())

	for _, tc := range []struct {
		filename, wantType string
		data               []byte
	}{
		{"page.html", "application/octet-stream", []byte("<html></html>")},
		{"logo.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		{"cv.pdf", "application/pdf", []byte("%PDF-1.7\n")},
	} {
		resp := postMedia(t, app, "", tc.filename, tc.data)
		if resp.status != fiber.StatusCreated || resp.body["purpose"] != media.PurposeLegacy || resp.body["type"] != tc.wantType {
			t.Errorf("%s: status %d body %v", tc.filename, resp.status, resp.body)
		}
	}

	resp := postMedia(t, app, "", "cv.txt", []byte("%PDF-1.7\n"))
	if resp.status != fiber.StatusBadRequest || resp.body["code"] != nil {
		t.Fatalf("PDF under a .txt name: status %d body %v", resp.status, resp.body)
	}
}

func TestMediaUploadNamingTheLegacyPurposeHTTP(t *testing.T) {
	t.Parallel()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "legacy", "page.html", []byte("<html></html>"))
	requireProblem(t, resp, fiber.StatusBadRequest, "purpose_unknown")
}

func TestMediaUploadReadsThePurposeOnlyFromTheFormHTTP(t *testing.T) {
	t.Parallel()
	organizer := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/YK"}}
	app := mediaApp(t, organizer, media.NewMemoryStore(), media.NewMemoryBlob())

	body, ctype := multipartPNG(t, "file", "cover.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media?purpose=event_cover", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated || got["purpose"] != media.PurposeLegacy {
		t.Fatalf("purpose in the query string: status %d body %v", resp.StatusCode, got)
	}
}
