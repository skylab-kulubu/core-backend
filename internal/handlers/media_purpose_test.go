package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
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
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if purpose != "" {
		if err := form.WriteField("purpose", purpose); err != nil {
			t.Fatal(err)
		}
	}
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
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
	requireProblem(t, resp, fiber.StatusBadRequest, "purpose-unknown")
	if resp.body["purpose"] != "banner" {
		t.Fatalf("problem %v", resp.body)
	}
}

func TestMediaUploadOfTheWrongTypeHTTP(t *testing.T) {
	t.Parallel()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "profile_picture", "me.png", []byte("%PDF-1.7\n"))
	requireProblem(t, resp, fiber.StatusUnsupportedMediaType, "media-type-not-allowed")
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
	requireProblem(t, resp, fiber.StatusRequestEntityTooLarge, "media-too-large")
	if resp.body["purpose"] != "profile_picture" || resp.body["maxBytes"] != float64(5<<20) {
		t.Fatalf("problem %v", resp.body)
	}
}

func TestMediaUploadBySomeoneThePurposeDoesNotAllowHTTP(t *testing.T) {
	t.Parallel()
	member := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	app := mediaApp(t, member, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "event_cover", "cover.png", pngDotHTTP())
	requireProblem(t, resp, fiber.StatusForbidden, "purpose-forbidden")
	if resp.body["purpose"] != "event_cover" {
		t.Fatalf("problem %v", resp.body)
	}
}

func TestMediaUploadForAPrivatePurposeWhilePrivateMediaIsOffHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, store, media.NewMemoryBlob())

	resp := postMedia(t, app, "answer_file", "cv.pdf", []byte("%PDF-1.7\n"))
	requireProblem(t, resp, fiber.StatusServiceUnavailable, "private-media-disabled")
	if stored, _ := store.List(t.Context()); len(stored) != 0 || resp.body["purpose"] != "answer_file" {
		t.Fatalf("problem %v, stored %d", resp.body, len(stored))
	}
}

func TestMediaUploadForADirectUploadPurposeHTTP(t *testing.T) {
	t.Parallel()
	organizer := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/YK"}}
	app := mediaApp(t, organizer, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "video", "talk.mp4", []byte("\x00\x00\x00\x18ftypmp42"))
	requireProblem(t, resp, fiber.StatusBadRequest, "purpose-requires-direct-upload")
	if resp.body["purpose"] != "video" {
		t.Fatalf("problem %v", resp.body)
	}
}

// Purpose-less uploads keep the rules they had before Media purpose until
// Skyforms and CMS send purposes (ADR-0052): any named file up to 20 MiB,
// SVG kept, a PDF only under a .pdf name, and no purpose codes.
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
