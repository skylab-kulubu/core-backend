package handlers

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
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
	requireProblem(t, resp, fiber.StatusRequestEntityTooLarge, "media_too_large")
	if resp.body["purpose"] != "profile_picture" || resp.body["maxPixels"] != float64(media.MaxImagePixels) || resp.body["maxBytes"] != float64(5<<20) {
		t.Fatalf("problem %v", resp.body)
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
	variants, _ := got["variants"].(map[string]any)
	card, _ := variants["card"].(map[string]any)
	page, _ := variants["page"].(map[string]any)
	url, _ := got["url"].(string)
	if got["width"] != float64(1000) || got["height"] != float64(800) {
		t.Fatalf("anonymous response %v", got)
	}
	if card["url"] != url+"/card" || card["width"] != float64(400) || card["height"] != float64(320) {
		t.Fatalf("card %v (image at %s)", card, url)
	}
	if page["url"] != url || page["width"] != float64(1000) {
		t.Fatalf("page %v, want the image itself", page)
	}
}
