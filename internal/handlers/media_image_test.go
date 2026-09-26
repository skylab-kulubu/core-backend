package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/png"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// pngClaimingHTTP is a PNG whose header claims w×h pixels over a few bytes
// of data.
func pngClaimingHTTP(w, h uint32) []byte {
	chunk := func(kind string, data []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
		out = append(out, kind...)
		out = append(out, data...)
		return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(kind), data...)))
	}
	ihdr := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, w), h)
	out := append([]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", append(ihdr, 8, 6, 0, 0, 0))...)
	out = append(out, chunk("IDAT", []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})...)
	return append(out, chunk("IEND", nil)...)
}

func grayPNGHTTP(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 128
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestMediaUploadWithMorePixelsThanCoreDecodesHTTP(t *testing.T) {
	t.Parallel()
	app := mediaApp(t, authn.Identity{ID: uuid.New()}, media.NewMemoryStore(), media.NewMemoryBlob())

	resp := postMedia(t, app, "profile_picture", "bomb.png", pngClaimingHTTP(30000, 30000))
	requireProblem(t, resp, fiber.StatusRequestEntityTooLarge, "media_image_too_large")
	if _, ok := resp.body["maxBytes"]; ok || resp.body["purpose"] != "profile_picture" || resp.body["maxPixels"] != float64(media.MaxImagePixels) {
		t.Fatalf("problem %v, want maxPixels alone", resp.body)
	}
}

func TestMediaGetGivesEveryoneTheAddressesOfAnImagesSizesHTTP(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	uploader := authn.Identity{ID: uuid.New()}
	created := postMedia(t, mediaApp(t, uploader, store, blobs), "profile_picture", "wide.png", grayPNGHTTP(t, 1000, 800))
	if created.status != fiber.StatusCreated {
		t.Fatalf("upload %d %v", created.status, created.body)
	}
	id, _ := uuid.Parse(created.body["id"].(string))

	got := getMediaJSON(t, mediaApp(t, authn.Identity{}, store, blobs), id)
	sizes, _ := got["sizes"].(map[string]any)
	card, _ := sizes["card"].(map[string]any)
	page, _ := sizes["page"].(map[string]any)
	url, _ := got["url"].(string)
	if got["width"] != float64(1000) || got["height"] != float64(800) {
		t.Fatalf("anonymous response %v", got)
	}
	if card["url"] != url+"/card.png" || card["width"] != float64(400) || card["height"] != float64(320) {
		t.Fatalf("card %v (image at %s)", card, url)
	}
	if page["url"] != url || page["width"] != float64(1000) {
		t.Fatalf("page %v, want the image itself", page)
	}
}

func TestMediaUploadAnswersBusyWhenNoDecodingSlotFreesUpHTTP(t *testing.T) {
	t.Parallel()
	budget := media.NewDecodeBudget(media.DecodeBudgetConfig{Slots: 1, Wait: 20 * time.Millisecond})
	svc := media.NewServiceWithOptions(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{DecodeBudget: budget})
	h := NewMediaHandler(svc)
	app := fiber.New(fiber.Config{BodyLimit: media.MaxUploadBytes + 1<<20})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.New()})
		return c.Next()
	})
	app.Post("/v1/media", h.Upload)

	release, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	body, contentType := multipartFile(t, map[string]string{"purpose": "profile_picture"}, "file", "me.png", pngDotHTTP())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", body)
	req.Header.Set("Content-Type", contentType)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusServiceUnavailable || got["code"] != "media_busy" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d Retry-After %q body %v", resp.StatusCode, resp.Header.Get("Retry-After"), got)
	}
}
