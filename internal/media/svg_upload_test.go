package media_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// svgService uploads cms_image, the purpose that rasterizes SVG, with core
// as its attacher: nothing else can attach it until the service attach API
// exists.
func svgService(t *testing.T) (media.Service, *media.MemoryBlob) {
	t.Helper()
	catalogue, err := media.ParseCatalogue(reviewedCatalogueWith(t, func(purposes purposeEntries) {
		purposes["cms_image"]["attach"] = "core"
	}))
	if err != nil {
		t.Fatal(err)
	}
	blobs := media.NewMemoryBlob()
	return media.NewServiceWithOptions(media.NewMemoryStore(), blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{Catalogue: catalogue}), blobs
}

const redLogo = `<?xml version="1.0"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 50">
  <script>alert(document.cookie)</script>
  <rect width="100" height="50" fill="#e60000" onclick="alert(1)"/>
</svg>`

func TestService_SVGIsStoredAsAPNGOfIt(t *testing.T) {
	t.Parallel()
	svc, blobs := svgService(t)

	created, err := svc.UploadForPurpose(context.Background(), signedIn("75757575-7575-7575-7575-757575757575"), "cms_image", uploaded("logo.svg", "image/svg+xml", []byte(redLogo)))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := blobs.Get(created.Key)
	for _, svg := range []string{"<svg", "script", "alert"} {
		if bytes.Contains(stored, []byte(svg)) {
			t.Errorf("stored object carries %q", svg)
		}
	}
	img, format := decodeStored(t, stored)
	if format != "png" || created.Type != "image/png" || img.Bounds().Size() != image.Pt(1200, 600) || created.Width != 1200 || created.Height != 600 {
		t.Fatalf("stored %s %v, recorded %s %d×%d; want a 1200×600 PNG", format, img.Bounds().Size(), created.Type, created.Width, created.Height)
	}
	if !isRed(img.At(600, 300)) {
		t.Fatalf("pixel %v, want the logo's red", img.At(600, 300))
	}
	if meta, _ := blobs.Metadata(created.Key); meta != (media.BlobMetadata{ContentType: "image/png"}) {
		t.Fatalf("served as %+v", meta)
	}
	if card := created.Variants["card"]; card.URL != created.URL+"/card" || card.Width != 400 || card.Height != 200 {
		t.Fatalf("card %+v", card)
	}
}

func TestService_SVGThatCoreDoesNotRasterizeIsRefused(t *testing.T) {
	t.Parallel()
	svc, blobs := svgService(t)
	p := signedIn("76767676-7676-7676-7676-767676767676")
	manyShapes := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">` + strings.Repeat(`<rect width="1" height="1"/>`, 10001) + `</svg>`

	for name, svg := range map[string]string{
		"a use inside defs, which can refer to itself": `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 10 10">
			<defs><g id="a"><use xlink:href="#a"/></g></defs><use xlink:href="#a"/></svg>`,
		"a document type":        `<!DOCTYPE svg [<!ENTITY a "aaaaaaaa">]><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><text>&a;</text></svg>`,
		"more shapes than drawn": manyShapes,
		"no size":                `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`,
		"not XML":                `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect`,
	} {
		_, err := svc.UploadForPurpose(context.Background(), p, "cms_image", uploaded("logo.svg", "image/svg+xml", []byte(svg)))
		if !errors.Is(err, media.ErrTypeNotAllowed) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrTypeNotAllowed)
		}
	}

	huge := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><!--` + strings.Repeat("x", 1<<20) + `--></svg>`
	_, err := svc.UploadForPurpose(context.Background(), p, "cms_image", uploaded("logo.svg", "image/svg+xml", []byte(huge)))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrTooLarge) || !errors.As(err, &refusal) || refusal.MaxBytes != 1<<20 {
		t.Fatalf("an SVG over 1 MiB: err = %v", err)
	}
	if left := blobs.Keys(); len(left) != 0 {
		t.Fatalf("refused SVGs left objects %v", left)
	}
}

func TestService_SVGForAPurposeThatDoesNotRasterizeIsRefused(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)

	_, err := svc.UploadForPurpose(context.Background(), signedIn("77777777-7777-7777-7777-000000000077"), "profile_picture", uploaded("me.svg", "image/svg+xml", []byte(redLogo)))
	if !errors.Is(err, media.ErrTypeNotAllowed) {
		t.Fatalf("err = %v, want %v", err, media.ErrTypeNotAllowed)
	}
}

// Not parallel: it shortens the process's time limit for drawing an SVG.
func TestService_SVGThatTakesTooLongToDrawIsRefused(t *testing.T) {
	svc, _ := svgService(t)
	p := signedIn("78787878-7878-7878-7878-787878787878")
	// Every shape costs a pass over the whole canvas.
	detailed := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">` + strings.Repeat(`<rect width="10" height="10" fill="#123456"/>`, 40) + `</svg>`)

	restore := media.LimitSVGDrawingTo(10 * time.Millisecond)
	start := time.Now()
	_, err := svc.UploadForPurpose(context.Background(), p, "cms_image", uploaded("detailed.svg", "image/svg+xml", detailed))
	elapsed := time.Since(start)
	restore()
	if !errors.Is(err, media.ErrTypeNotAllowed) {
		t.Fatalf("err = %v, want %v", err, media.ErrTypeNotAllowed)
	}
	if elapsed > time.Second {
		t.Fatalf("refused after %s", elapsed)
	}
	if _, err := svc.UploadForPurpose(context.Background(), p, "cms_image", uploaded("detailed.svg", "image/svg+xml", detailed)); err != nil {
		t.Fatalf("the same SVG within the usual limit: %v", err)
	}
}
